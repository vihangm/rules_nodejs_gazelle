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
	"io/ioutil"
	"log"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

type JS struct {
	Config       *JsConfig
	WebResources map[string]bool
}

type imports struct {
	set map[string]bool
}

var noImports = imports{
	set: map[string]bool{},
}

func NewLanguage() language.Language {
	return &JS{
		Config:       NewJsConfig(),
		WebResources: make(map[string]bool),
	}
}

// Kinds returns a map of maps rule names (kinds) and information on how to
// match and merge attributes that may be found in rules of those kinds. All
// kinds of rules generated for this language may be found here.
func (*JS) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"js_library": {
			MatchAny: false,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
			ResolveAttrs: map[string]bool{
				"deps": true,
				"data": true,
			},
		},
		"ts_project": {
			MatchAny: false,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
			ResolveAttrs: map[string]bool{
				"deps": true,
				"data": true,
			},
		},
		"ts_definition": {
			MatchAny: false,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
			ResolveAttrs: map[string]bool{
				"deps": true,
				"data": true,
			},
		},
		"jest_test": {
			MatchAny: false,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
			ResolveAttrs: map[string]bool{
				"deps": true,
				"data": true,
			},
		},
		"web_asset": {
			MatchAny: true,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
		},
		"web_assets": {
			MatchAny: true,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			MergeableAttrs: map[string]bool{
				"srcs": true,
				"tags": true,
			},
		},
	}
}

var managedRules = []string{"js_library", "ts_project", "jest_test", "web_asset", "web_assets", "ts_definition"}
var managedRulesSet map[string]bool

func init() {
	managedRulesSet = make(map[string]bool)
	for _, rule := range managedRules {
		managedRulesSet[rule] = true
	}
}

// Loads returns .bzl files and symbols they define. Every rule generated by
// GenerateRules, now or in the past, should be loadable from one of these
// files.
func (lang *JS) Loads() []rule.LoadInfo {

	loads := []rule.LoadInfo{}

	// This need to be hacked in from os.Args because Loads() is called before Flags processing
	loadFromPattern := regexp.MustCompile(`^-load_from=(.+)$`)
	for _, arg := range os.Args {
		match := loadFromPattern.FindStringSubmatch(arg)
		if len(match) > 0 {
			loads = append(loads, rule.LoadInfo{
				Name:    string(match[1]),
				Symbols: managedRules,
			},
			)
		}
	}

	// default
	if len(loads) == 0 {
		loads = []rule.LoadInfo{{
			Name:    "@com_github_benchsci_rules_nodejs_gazelle//:defs.bzl",
			Symbols: managedRules,
		}}
	}

	return loads
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
//
func (lang *JS) GenerateRules(args language.GenerateArgs) language.GenerateResult {

	for _, pattern := range lang.Config.Ignores.Patterns {
		if pattern.MatchString(args.Rel + "/") {
			// ignore this directory
			return language.GenerateResult{}
		}
	}

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

	pkgName := PkgName(args.Rel)

	managedFiles := make(map[string]bool)
	webAssetsSet := make(map[string]bool)

	tsSources := []string{}
	tsImports := []imports{}
	jsSources := []string{}
	jsImports := []imports{}

	generatedRules := make([]*rule.Rule, 0)
	generatedImports := make([]interface{}, 0)

	module := false

	isWebRoot := path.Clean(lang.Config.WebRoot) == args.Rel

	for _, baseName := range append(args.RegularFiles, args.GenFiles...) {
		managedFiles[baseName] = true

		filePath := path.Join(args.Dir, baseName)

		// TS DEFINITIONS ".d.ts"
		match := tsDefsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			r := rule.NewRule("ts_definition", strings.TrimSuffix(baseName, match[0])+".d")
			r.SetAttr("srcs", []string{baseName})
			r.SetAttr("visibility", lang.Config.Visibility.Labels)

			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, &noImports)
			continue
		}

		// JS TEST
		match = jsTestExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			i, r := lang.makeTestRule(testRuleArgs{
				ruleType:  "jest_test",
				extension: match[0],
				filePath:  filePath,
				baseName:  baseName,
			})
			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, i)
			continue
		}
		// TS TEST
		match = tsTestExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			i, r := lang.makeTestRule(testRuleArgs{
				ruleType:  "jest_test",
				extension: match[0],
				filePath:  filePath,
				baseName:  baseName,
			})
			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, i)
			continue
		}

		// if the filename is like index.(jsx) then we assume we found a module
		if isModuleFile(baseName) {
			module = true
		}

		// TS
		match = tsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			tsSources = append(tsSources, baseName)
			tsImports = append(tsImports, *readFileAndParse(filePath))
			continue
		}
		// JS
		match = jsExtensionsPattern.FindStringSubmatch(baseName)
		if len(match) > 0 {
			jsSources = append(jsSources, baseName)
			jsImports = append(jsImports, *readFileAndParse(filePath))
			continue
		}

		// OTHER FILE
		if baseName != "BUILD" {
			webAssetsSet[baseName] = true
			continue
		}
	}

	if module && len(tsSources) > 0 && len(jsSources) > 0 {
		log.Printf("[WARN] ts and js files mixed in module %s", pkgName)
	}

	aggregateModule := lang.Config.AggregateModules && module
	if aggregateModule {
		for _, pattern := range lang.Config.NoAggregateLike.Patterns {
			if pattern.MatchString(args.Rel + "/") {
				// Do not aggregate this module
				aggregateModule = false
				break
			}
		}
	}

	// add "ts_project" rule(s)
	if len(tsSources) > 0 {
		name := pkgName
		if len(jsSources) > 0 {
			name = name + ".ts"
		}
		if aggregateModule {
			// add as a module
			i, r := lang.makeModuleRule(moduleRuleArgs{
				ruleName: name,
				ruleType: "ts_project",
				srcs:     tsSources,
				imports:  tsImports,
			})
			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, i)
		} else {
			// add as singletons
			tsRules := lang.makeRules(ruleArgs{
				ruleType: "ts_project",
				srcs:     tsSources,
				trimExt:  true,
			})
			for i := range tsRules {
				generatedRules = append(generatedRules, tsRules[i])
				generatedImports = append(generatedImports, &tsImports[i])
			}
		}
	}

	// add "js_library" rule(s)
	if len(jsSources) > 0 {
		if aggregateModule {
			// add as a module
			i, r := lang.makeModuleRule(moduleRuleArgs{
				ruleName: pkgName,
				ruleType: "js_library",
				srcs:     jsSources,
				imports:  jsImports,
			})
			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, i)
		} else {
			// add as singletons
			jsRules := lang.makeRules(ruleArgs{
				ruleType: "js_library",
				srcs:     jsSources,
				trimExt:  true,
			})

			for i := range jsRules {
				generatedRules = append(generatedRules, jsRules[i])
				generatedImports = append(generatedImports, &jsImports[i])
			}
		}
	}

	// read webAssetsSet to list
	webAssets := make([]string, 0, len(webAssetsSet))
	for fl := range webAssetsSet {
		webAssets = append(webAssets, fl)
	}
	if len(webAssets) > 0 {
		// Generate web_asset rule(s)

		if lang.Config.AggregateWebAssets {
			// aggregate rule
			name := "assets"
			r := rule.NewRule("web_assets", name)
			r.SetAttr("srcs", webAssets)
			r.SetAttr("visibility", lang.Config.Visibility.Labels)

			generatedRules = append(generatedRules, r)
			generatedImports = append(generatedImports, &noImports)

			// record all webAssets rules for all_assets rule later
			fqName := fmt.Sprintf("//%s:%s", path.Join(args.Rel), name)
			lang.WebResources[fqName] = true

		} else {
			// add as singletons
			rules := lang.makeRules(ruleArgs{
				ruleType: "web_asset",
				srcs:     webAssets,
				trimExt:  false, //shadow the original file name
			})

			for _, r := range rules {
				generatedRules = append(generatedRules, r)
				generatedImports = append(generatedImports, &noImports)

				// record all webAssets rules for all_assets rule later
				fqName := fmt.Sprintf("//%s:%s", path.Join(args.Rel), r.Name())
				lang.WebResources[fqName] = true
			}
		}
	}

	if isWebRoot && lang.Config.AggregateAllAssets {
		// Generate all_assets rule
		webRootDeps := []string{}
		for fqName := range lang.WebResources {
			webRootDeps = append(webRootDeps, fqName)
		}
		name := "all_assets"
		r := rule.NewRule("web_assets", name)
		r.SetAttr("srcs", webRootDeps)

		generatedRules = append(generatedRules, r)
		generatedImports = append(generatedImports, &noImports)
	}

	// Generate a list of rules that may be deleted
	// This is generated from existing rules that are managed by gazelle
	// that didn't get generated this run
	deleteRulesSet := existingRules // no need to copy

	for _, generatedRule := range generatedRules {
		name := generatedRule.Name()
		// This is not an empty rule
		delete(deleteRulesSet, name)
	}

	for _, r := range deleteRulesSet {
		// Is this rule managed by Gazelle?
		if _, ok := managedRulesSet[r.Kind()]; ok {
			// It is managed, and wasn't generated, so delete it
			r.Delete()
		}
	}

	return language.GenerateResult{
		Gen:     generatedRules,
		Empty:   []*rule.Rule{},
		Imports: generatedImports,
	}
}

// Fix repairs deprecated usage of language-specific rules in f. This is
// called before the file is indexed. Unless c.ShouldFix is true, fixes
// that delete or rename rules should not be performed.
func (*JS) Fix(c *config.Config, f *rule.File) {
	if c.ShouldFix {
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

type testRuleArgs struct {
	ruleType  string
	extension string
	filePath  string
	baseName  string
}

func (lang *JS) makeTestRule(args testRuleArgs) (*imports, *rule.Rule) {
	imps := readFileAndParse(args.filePath)
	ruleName := strings.TrimSuffix(args.baseName, args.extension) + ".test"
	r := rule.NewRule(args.ruleType, ruleName)
	r.SetAttr("srcs", []string{args.baseName})
	r.SetAttr("visibility", lang.Config.Visibility.Labels)
	return imps, r
}

type moduleRuleArgs struct {
	ruleName string
	ruleType string
	srcs     []string
	imports  []imports
}

func (lang *JS) makeModuleRule(args moduleRuleArgs) (*imports, *rule.Rule) {
	imps := aggregateImports(args.imports)
	r := rule.NewRule(args.ruleType, args.ruleName)
	r.SetAttr("srcs", args.srcs)
	r.SetAttr("visibility", lang.Config.Visibility.Labels)
	r.SetAttr("tags", []string{"js_module"})
	return imps, r
}

type ruleArgs struct {
	ruleType string
	srcs     []string
	trimExt  bool
}

func (lang *JS) makeRules(args ruleArgs) []*rule.Rule {
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
		r.SetAttr("visibility", lang.Config.Visibility.Labels)
		rules = append(rules, r)
	}
	return rules
}

func readFileAndParse(filePath string) *imports {

	fileImports := imports{
		set: make(map[string]bool),
	}

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Fatalf("Error reading %s: %v", filePath, err)
	}
	jsImports, err := ParseJS(data)
	if err != nil {
		log.Fatalf("Error parsing %s: %v", filePath, err)
	}
	for _, imp := range jsImports {
		fileImports.set[imp] = true
	}

	return &fileImports
}

func aggregateImports(imps []imports) *imports {

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
