// Copyright 2019 The Bazel Authors. All rights reserved.
// Modifications copyright (C) 2021 BenchSci Analytics Inc.
// Modifications copyright (C) 2018 Ecosia GmbH

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package js

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

type imports struct {
	set map[string]bool
}

var noImports = imports{
	set: map[string]bool{},
}

var localRules = rule.LoadInfo{
	Name:    "@com_github_benchsci_rules_nodejs_gazelle//:defs.bzl",
	Symbols: []string{"web_asset", "web_assets", "js_library", "ts_definition"},
}
var tsRules = rule.LoadInfo{
	Name:    "@npm//@bazel/typescript:index.bzl",
	Symbols: []string{"ts_project"},
}
var jestRules = rule.LoadInfo{
	Name:    "@npm//jest:index.bzl",
	Symbols: []string{"jest_test"},
}
var managedRulesSet map[string]bool

func init() {
	managedRulesSet = make(map[string]bool)
	for _, rule := range localRules.Symbols {
		managedRulesSet[rule] = true
	}
	for _, rule := range tsRules.Symbols {
		managedRulesSet[rule] = true
	}
	for _, rule := range jestRules.Symbols {
		managedRulesSet[rule] = true
	}
}

// Loads returns .bzl files and symbols they define. Every rule generated by
// GenerateRules, now or in the past, should be loadable from one of these
// files.
func (lang *JS) Loads() []rule.LoadInfo {
	return []rule.LoadInfo{
		localRules,
		tsRules,
		jestRules,
	}
}

func getKind(c *config.Config, kindName string) string {
	// Extract kind_name from KindMap
	if kind, ok := c.KindMap[kindName]; ok {
		return kind.KindName

	}
	return kindName
}

// GenerateRules extracts build metadata from source files in a directory.
// GenerateRules is called in each directory where an update is requested
// in depth-first post-order.
//
// args contains the arguments for GenerateRules. This is passed as a
// struct to avoid breaking implementations in the future when new
// fields are added.
//
// A GenerateResult struct is returned. Optional fields may be added to this
// type in the future.
func (lang *JS) GenerateRules(args language.GenerateArgs) language.GenerateResult {

	jsConfigs := args.Config.Exts[languageName].(JsConfigs)
	jsConfig := jsConfigs[args.Rel]

	if !jsConfig.Enabled {
		// ignore this directory
		return language.GenerateResult{}
	}

	pkgName := PkgName(args.Rel)

	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	var tsdSources,
		jestSources,
		tsSources,
		jsSources,
		webAssetsSet,
		isModule,
		isJSRoot = lang.collectSources(args, jsConfig)

	if isModule && len(tsSources) > 0 && len(jsSources) > 0 {
		log.Print(Warn("[WARN] ts and js files mixed in module %s", pkgName))
	}

	// add "ts_definition" rule(s)
	generatedTSDRules, generatedTSDImports := lang.genTSDefinition(args, jsConfig, tsdSources)
	generatedRules = append(generatedRules, generatedTSDRules...)
	generatedImports = append(generatedImports, generatedTSDImports...)

	// add "jest_test" rule(s)
	generatedTestRules, generatedTestImports := lang.genJestTest(args, jsConfig, jestSources)
	generatedRules = append(generatedRules, generatedTestRules...)
	generatedImports = append(generatedImports, generatedTestImports...)

	appendTSExt := len(jsSources) > 0

	if len(jsSources) > 0 && jsConfig.FolderAsRule {
		// add combined "ts_project" rule with js sources
		generatedTSRules, generatedTSImports := lang.genRules(args, jsConfig, isModule, isJSRoot, pkgName, append(tsSources, jsSources...), false, "ts_project")
		generatedRules = append(generatedRules, generatedTSRules...)
		generatedImports = append(generatedImports, generatedTSImports...)

	} else {
		// add "ts_project" rule(s)
		generatedTSRules, generatedTSImports := lang.genRules(args, jsConfig, isModule, isJSRoot, pkgName, tsSources, appendTSExt, "ts_project")
		generatedRules = append(generatedRules, generatedTSRules...)
		generatedImports = append(generatedImports, generatedTSImports...)

		// add "js_library" rule(s)
		appendTSExt = false
		generatedJSRules, generatedJSImports := lang.genRules(args, jsConfig, isModule, isJSRoot, pkgName, jsSources, appendTSExt, "js_library")
		generatedRules = append(generatedRules, generatedJSRules...)
		generatedImports = append(generatedImports, generatedJSImports...)
	}

	// add "web_asset" rule(s)
	generatedWARules, generatedWAImports := lang.genWebAssets(args, webAssetsSet, jsConfig)
	generatedRules = append(generatedRules, generatedWARules...)
	generatedImports = append(generatedImports, generatedWAImports...)

	// add "all_assets" "web_assets" rule
	generatedAWARules, generatedAWAImports := lang.genAllAssets(args, isJSRoot, jsConfig)
	generatedRules = append(generatedRules, generatedAWARules...)
	generatedImports = append(generatedImports, generatedAWAImports...)

	existingRules := lang.readExistingRules(args)
	lang.pruneManagedRules(existingRules, generatedRules)

	return language.GenerateResult{
		Gen:     generatedRules,
		Empty:   []*rule.Rule{},
		Imports: generatedImports,
	}
}

func (lang *JS) collectSources(args language.GenerateArgs, jsConfig *JsConfig) ([]string, []string, []string, []string, map[string]bool, bool, bool) {

	managedFiles := make(map[string]bool)
	tsdSources := []string{}
	jestSources := []string{}
	tsSources := []string{}
	jsSources := []string{}
	webAssetsSet := make(map[string]bool)

	isModule := false

	absJSRoot, _ := filepath.Abs(jsConfig.JSRoot)
	isJSRoot := absJSRoot == args.Dir

	for _, baseName := range lang.gatherFiles(args, jsConfig) {

		managedFiles[baseName] = true

		// TS DEFINITIONS ".d.ts"
		match := tsDefsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			tsdSources = append(tsdSources, baseName)
			continue
		}

		// TS & JS TEST
		match = append(jsTestExtensionsPattern.FindStringSubmatch(baseName), tsTestExtensionsPattern.FindStringSubmatch(baseName)...)
		if len(match) > 0 {
			jestSources = append(jestSources, baseName)
			continue
		}

		// if the filename is like index.(jsx) then we assume we found a module
		if isModuleFile(baseName) {
			isModule = true
		}

		// TS
		match = tsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			tsSources = append(tsSources, baseName)
			continue
		}
		// JS
		match = jsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			jsSources = append(jsSources, baseName)
			continue
		}

		// WEB ASSETS
		for suffix := range jsConfig.WebAssetSuffixes {
			if strings.HasSuffix(baseName, suffix) {
				webAssetsSet[baseName] = true
				continue
			}
		}

	}

	return tsdSources,
		jestSources,
		tsSources,
		jsSources,
		webAssetsSet,
		isModule,
		isJSRoot
}

func (lang *JS) gatherFiles(args language.GenerateArgs, jsConfig *JsConfig) []string {
	allFiles := args.RegularFiles
	if jsConfig.FolderAsRule {
		for _, subDir := range args.Subdirs {
			relDir := path.Join(args.Dir, subDir)
			filepath.Walk(relDir, func(path string, info fs.FileInfo, err error) error {
				if info != nil && !info.IsDir() {
					allFiles = append(allFiles,
						strings.TrimPrefix(strings.TrimPrefix(path, args.Dir), "/"),
					)
				}
				return nil
			})
		}
	}
	return allFiles
}

func readFileAndParse(filePath string, rel string) *imports {

	fileImports := imports{
		set: make(map[string]bool),
	}

	// If this file is a React component, always add react as dependency as the file could be using native
	// JSX transpilation from React package that doesn't need the "import React" statement
	if isReactFile(filePath) {
		fileImports.set["react"] = true
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf(Err("Error reading %s: %v", filePath, err))
	}
	jsImports, err := ParseJS(data)
	if err != nil {
		log.Fatalf(Err("Error parsing %s: %v", filePath, err))
	}
	for _, imp := range jsImports {
		if rel != "" && strings.HasPrefix(imp, ".") {
			imp = path.Join(rel, imp)
		}
		fileImports.set[imp] = true
	}

	return &fileImports
}

func (lang *JS) genTSDefinition(args language.GenerateArgs, jsConfig *JsConfig, tsdSources []string) ([]*rule.Rule, []interface{}) {
	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	for _, baseName := range tsdSources {
		match := tsDefsExtensionsPattern.FindStringSubmatch(baseName)
		r := rule.NewRule(getKind(args.Config, "ts_definition"), strings.TrimSuffix(baseName, match[0])+".d")
		r.SetAttr("srcs", []string{baseName})
		if len(jsConfig.Visibility.Labels) > 0 {
			r.SetAttr("visibility", jsConfig.Visibility.Labels)
		}
		generatedRules = append(generatedRules, r)
		generatedImports = append(generatedImports, &noImports)
	}

	return generatedRules, generatedImports
}

func (lang *JS) genJestTest(args language.GenerateArgs, jsConfig *JsConfig, jestSources []string) ([]*rule.Rule, []interface{}) {
	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	if !jsConfig.FolderAsRule {
		// Add each test as an individual rule
		for _, baseName := range jestSources {
			match := append(jsTestExtensionsPattern.FindStringSubmatch(baseName), tsTestExtensionsPattern.FindStringSubmatch(baseName)...)
			filePath := path.Join(args.Dir, baseName)
			extension := match[0]

			r := rule.NewRule(
				getKind(args.Config, "jest_test"),
				strings.TrimSuffix(baseName, extension)+".test",
			)
			r.SetAttr("srcs", []string{baseName})
			if jsConfig.TestSize != "" {
				r.SetAttr("size", jsConfig.TestSize)
			}
			if len(jsConfig.Visibility.Labels) > 0 {
				r.SetAttr("visibility", jsConfig.Visibility.Labels)
			}

			imports := readFileAndParse(filePath, "")

			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, imports)
		}

	} else if len(jestSources) > 0 {
		// Add all tests as a single rule
		var allImports []imports
		for _, baseName := range jestSources {
			filePath := path.Join(args.Dir, baseName)
			relativePart := path.Dir(baseName)
			allImports = append(allImports, *readFileAndParse(filePath, relativePart))
		}
		imports := flattenImports(allImports)

		pkgName := PkgName(args.Rel)
		ruleName := fmt.Sprintf("%s_test", pkgName)
		r := rule.NewRule(
			getKind(args.Config, "jest_test"),
			ruleName,
		)

		r.SetAttr("srcs", jestSources)
		if jsConfig.TestShards > 0 {
			r.SetAttr("shard_count", jsConfig.TestShards)
		}
		if jsConfig.TestSize != "" {
			r.SetAttr("size", jsConfig.TestSize)
		}
		if len(jsConfig.Visibility.Labels) > 0 {
			r.SetAttr("visibility", jsConfig.Visibility.Labels)
		}

		generatedRules = append(generatedRules, r)
		generatedImports = append(generatedImports, imports)
	}

	return generatedRules, generatedImports
}

type testRuleArgs struct {
	ruleType  string
	extension string
	filePath  string
	baseName  string
}

func (lang *JS) makeFolderTestRule(args testRuleArgs, jsConfig *JsConfig) (*imports, *rule.Rule) {
	imps := readFileAndParse(args.filePath, "")
	ruleName := strings.TrimSuffix(args.baseName, args.extension) + ".test"
	r := rule.NewRule(args.ruleType, ruleName)
	r.SetAttr("srcs", []string{args.baseName})
	if len(jsConfig.Visibility.Labels) > 0 {
		r.SetAttr("visibility", jsConfig.Visibility.Labels)
	}
	return imps, r
}

func (lang *JS) genRules(args language.GenerateArgs, jsConfig *JsConfig, isModule bool, isJSRoot bool, pkgName string, sources []string, appendTSExt bool, kind string) ([]*rule.Rule, []interface{}) {

	// Parse files to get imports
	var imports []imports
	for _, baseName := range sources {
		filePath := path.Join(args.Dir, baseName)
		relativePart := ""
		if jsConfig.FolderAsRule {
			relativePart = path.Dir(baseName)
		}
		imports = append(imports, *readFileAndParse(filePath, relativePart))
	}

	aggregateModule := jsConfig.AggregateModules && isModule && !isJSRoot

	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	if len(sources) > 0 {
		name := pkgName
		if appendTSExt {
			name = name + ".ts"
		}
		if jsConfig.FolderAsRule {
			// add as a folder
			folderImports, folderRule := lang.makeFolderRule(moduleRuleArgs{
				pkgName:  name,
				cwd:      args.Rel,
				ruleType: getKind(args.Config, kind),
				srcs:     sources,
				imports:  imports,
			}, jsConfig)
			generatedRules = append(generatedRules, folderRule)
			generatedImports = append(generatedImports, folderImports)

		} else if aggregateModule {
			// add as a module (barrel file)
			moduleImports, moduleRules := lang.makeModuleRules(moduleRuleArgs{
				pkgName:  name,
				cwd:      args.Rel,
				ruleType: getKind(args.Config, kind),
				srcs:     sources,
				imports:  imports,
			}, jsConfig)
			if !jsConfig.Quiet && len(moduleRules) > 1 {
				log.Print(Warn("[WARN] disjoint module %s", args.Rel))
			}
			for i := range moduleRules {
				generatedRules = append(generatedRules, moduleRules[i])
				generatedImports = append(generatedImports, moduleImports[i])
			}
		} else {
			// add as singletons
			singletonRules := lang.makeRules(ruleArgs{
				ruleType: getKind(args.Config, kind),
				srcs:     sources,
				trimExt:  true,
			}, jsConfig)
			for i := range singletonRules {
				generatedRules = append(generatedRules, singletonRules[i])
				generatedImports = append(generatedImports, &imports[i])
			}
		}
	}

	return generatedRules, generatedImports
}

type ruleArgs struct {
	ruleType string
	srcs     []string
	trimExt  bool
}

func (lang *JS) makeRules(args ruleArgs, jsConfig *JsConfig) []*rule.Rule {
	rules := []*rule.Rule{}
	for _, src := range args.srcs {
		var name string
		if args.trimExt {
			name = trimExt(src)
		} else {
			name = strings.ReplaceAll(src, ".", "_")
			if name == src {
				name += ".file"
			}
		}
		r := rule.NewRule(args.ruleType, name)
		r.SetAttr("srcs", []string{src})
		if len(jsConfig.Visibility.Labels) > 0 {
			r.SetAttr("visibility", jsConfig.Visibility.Labels)
		}
		rules = append(rules, r)
	}
	return rules
}

type moduleRuleArgs struct {
	pkgName  string
	cwd      string
	ruleType string
	srcs     []string
	imports  []imports
}

func (lang *JS) makeModuleRules(args moduleRuleArgs, jsConfig *JsConfig) ([]*imports, []*rule.Rule) {

	// identify the "index.js|ts" src file and include it in moduleSet
	// all other source files start in remainderSet
	indexKey := ""
	moduleSet := make(map[string]imports)
	remainderSet := make(map[string]imports)

	for i, src := range args.srcs {
		if isModuleFile(src) {
			moduleSet[src] = args.imports[i]
			indexKey = src
		} else {
			remainderSet[src] = args.imports[i]
		}
	}

	// recurse through each import for src file
	// which, if it's local, transitively belongs in the moduleSet
	recAddTransitiveSet := func(src string) {
		// for each import that the source file has
		for imp, _ := range moduleSet[src].set {
			// if that import is local to this directory
			if isLocalImport(args.cwd, imp) {
				// check the remainderSet to see if the src file corresponding to the import
				// exists and also hasn't been included in the module yet
				basename := path.Base(imp)

				for _, ext := range append(tsExtensions, jsExtensions...) {
					filename := basename + ext
					if _, ok := remainderSet[filename]; ok {
						// copy the src file out of the remainderSet and into the moduleSet
						moduleSet[filename] = remainderSet[filename]
						delete(remainderSet, filename)
						break
					}
				}
			}
		}
	}

	// start with index and recurse through imports
	recAddTransitiveSet(indexKey)

	// Accumulate Modules sources and imports into lists
	moduleSrcs := make([]string, 0)
	moduleImportsList := make([]imports, 0)
	for src, imports := range moduleSet {
		moduleSrcs = append(moduleSrcs, src)
		moduleImportsList = append(moduleImportsList, imports)
	}
	moduleImports := flattenImports(moduleImportsList)

	// Use lists to make a rule
	moduleRule := rule.NewRule(args.ruleType, args.pkgName)
	moduleRule.SetAttr("srcs", moduleSrcs)
	if len(jsConfig.Visibility.Labels) > 0 {
		moduleRule.SetAttr("visibility", jsConfig.Visibility.Labels)
	}
	moduleRule.SetAttr("tags", []string{"js_module"})

	// Accumulate remainder srcs and imports into lists
	remainderSrcs := make([]string, 0)
	remainderImportsList := make([]imports, 0)
	keys := make([]string, 0)
	for k, _ := range remainderSet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		remainderSrcs = append(remainderSrcs, k)
		remainderImportsList = append(remainderImportsList, remainderSet[k])
	}

	// Make remainder rules
	remainderRules := lang.makeRules(ruleArgs{
		ruleType: args.ruleType,
		srcs:     remainderSrcs,
		trimExt:  true,
	}, jsConfig)

	// Collate results
	allImports := []*imports{moduleImports}
	allRules := []*rule.Rule{moduleRule}

	// Make a copy of imp to dereference
	for _, imp := range remainderImportsList {
		copyImp := imp
		copyImp.set = make(map[string]bool)
		for k, v := range imp.set {
			copyImp.set[k] = v
		}
		allImports = append(allImports, &copyImp) // Required to create references
	}
	allRules = append(allRules, remainderRules...)

	return allImports, allRules
}

func (lang *JS) makeFolderRule(args moduleRuleArgs, jsConfig *JsConfig) (*imports, *rule.Rule) {

	moduleImports := flattenImports(args.imports)

	// Use lists to make a rule
	moduleRule := rule.NewRule(args.ruleType, args.pkgName)
	moduleRule.SetAttr("srcs", args.srcs)
	if len(jsConfig.Visibility.Labels) > 0 {
		moduleRule.SetAttr("visibility", jsConfig.Visibility.Labels)
	}
	moduleRule.SetAttr("tags", []string{"js_folder"})

	return moduleImports, moduleRule
}

func isLocalImport(cwd string, path string) bool {

	// Special case for dot prefix without a folder
	if strings.HasPrefix(path, "./") {
		trimmed := strings.TrimPrefix(path, "./")
		return len(strings.Split(trimmed, "/")) == 1
	}

	// Compare both import path with cwd path
	// so "foo/bar/baz" matches "bar/baz/file.ts"
	cwdSegments := strings.Split(cwd, "/")
	importSegments := strings.Split(path, "/")
	for i := 0; i < len(importSegments)-1; i++ {
		j := len(importSegments) - i - 2 // ith deepest folder, minus file
		k := len(cwdSegments) - i - 1    // ith deepest folder
		if j < 0 || k < 0 || importSegments[j] != cwdSegments[k] {
			return false
		}
	}
	return true
}

func flattenImports(imps []imports) *imports {

	aggregatedImports := imports{
		set: make(map[string]bool),
	}
	for i := range imps {
		for k, v := range imps[i].set {
			aggregatedImports.set[k] = v
		}
	}

	return &aggregatedImports
}

func (lang *JS) genWebAssets(args language.GenerateArgs, webAssetsSet map[string]bool, jsConfig *JsConfig) ([]*rule.Rule, []interface{}) {

	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	// read webAssetsSet to list
	webAssets := make([]string, 0, len(webAssetsSet))
	for fl := range webAssetsSet {
		webAssets = append(webAssets, fl)
	}
	// always deterministic results
	sort.Strings(webAssets)

	if len(webAssets) > 0 {
		// Generate web_asset rule(s)

		if jsConfig.AggregateWebAssets {
			// aggregate rule
			name := "assets"
			r := rule.NewRule(getKind(args.Config, "web_assets"), name)
			r.SetAttr("srcs", webAssets)
			if len(jsConfig.Visibility.Labels) > 0 {
				r.SetAttr("visibility", jsConfig.Visibility.Labels)
			}

			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, &noImports)

			// record all webAssets rules for all_assets rule later
			fqName := fmt.Sprintf("//%s:%s", path.Join(args.Rel), name)
			jsConfig.AggregatedAssets[fqName] = true

		} else {
			// add as singletons
			rules := lang.makeRules(ruleArgs{
				ruleType: getKind(args.Config, "web_asset"),
				srcs:     webAssets,
				trimExt:  false, //shadow the original file name
			}, jsConfig)

			for _, r := range rules {
				generatedRules = append(generatedRules, r)
				generatedImports = append(generatedImports, &noImports)

				// record all webAssets rules for all_assets rule later
				fqName := fmt.Sprintf("//%s:%s", path.Join(args.Rel), r.Name())
				jsConfig.AggregatedAssets[fqName] = true
			}
		}
	}

	return generatedRules, generatedImports
}

func (lang *JS) genAllAssets(args language.GenerateArgs, isJSRoot bool, jsConfig *JsConfig) ([]*rule.Rule, []interface{}) {
	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	if isJSRoot && jsConfig.AggregateAllAssets {
		// Generate all_assets rule
		JSRootDeps := []string{}
		for fqName := range jsConfig.AggregatedAssets {
			JSRootDeps = append(JSRootDeps, fqName)
		}
		name := "all_assets"
		r := rule.NewRule(getKind(args.Config, "web_assets"), name)
		r.SetAttr("srcs", JSRootDeps)

		generatedRules = append(generatedRules, r)
		generatedImports = append(generatedImports, &noImports)
	}

	return generatedRules, generatedImports
}

func (lang *JS) readExistingRules(args language.GenerateArgs) map[string]*rule.Rule {
	existingRules := make(map[string]*rule.Rule)

	// BUILD file exists?
	if BUILD := args.File; BUILD != nil {
		// For each existing rule
		for _, r := range BUILD.Rules {
			if _, ok := managedRulesSet[r.Kind()]; !ok {
				// not a managed rule
				continue
			}
			existingRules[r.Name()] = r
		}
	}
	return existingRules
}

func (lang *JS) pruneManagedRules(existingRules map[string]*rule.Rule, generatedRules []*rule.Rule) {
	// Generate a list of rules that may be deleted and mark them for deletion
	// This is generated from existing rules that are managed by gazelle
	// that didn't get generated this run

	// Populate set with existing rules
	deleteRulesSet := make(map[string]*rule.Rule)
	for _, existingRule := range existingRules {
		// use kind/name to enable deletion of old rules when a new rule would use the same name
		key := fmt.Sprintf("%s/%s", existingRule.Kind(), existingRule.Name())
		deleteRulesSet[key] = existingRule
	}

	// Prune generated rules
	for _, generatedRule := range generatedRules {
		key := fmt.Sprintf("%s/%s", generatedRule.Kind(), generatedRule.Name())
		delete(deleteRulesSet, key)
	}

	for _, r := range deleteRulesSet {
		// Is this rule managed by Gazelle?
		if _, ok := managedRulesSet[r.Kind()]; ok {
			// It is managed, and wasn't generated, so delete it
			r.Delete()
		}
	}
}

// Fix repairs deprecated usage of language-specific rules in f. This is
// called before the file is indexed. Unless c.ShouldFix is true, fixes
// that delete or rename rules should not be performed.Í
func (*JS) Fix(c *config.Config, f *rule.File) {

	jsConfigs := c.Exts[languageName].(JsConfigs)
	jsConfig := jsConfigs[f.Pkg]

	if c.ShouldFix || jsConfig.Fix {
		for _, r := range f.Rules {
			// delete deprecated js_import rule
			if r.Kind() == "js_import" {
				r.Delete()
			}
			// delete deprecated ts_library rule
			if r.Kind() == "ts_library" {
				r.Delete()
			}
		}
		for _, l := range f.Loads {

			if l.Has("js_import") {
				l.Remove("js_import")
			}
			if l.Has("ts_library") {
				l.Remove("ts_library")
			}
		}
	}
}
