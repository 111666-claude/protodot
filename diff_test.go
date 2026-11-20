// Copyright 2017 Seamia Corporation. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/seamia/protodot/plus"
	"github.com/seamia/tools/support"
)

const (
	oldSchemaRoot = "testdata/diff/old/shop.proto"
	newSchemaRoot = "testdata/diff/new/shop.proto"
)

func collectBoth(t *testing.T) (*schema, *schema) {
	t.Helper()
	oldSchema, err := collectSchema(oldSchemaRoot)
	if err != nil {
		t.Fatalf("failed to collect the old schema: %v", err)
	}
	newSchema, err := collectSchema(newSchemaRoot)
	if err != nil {
		t.Fatalf("failed to collect the new schema: %v", err)
	}
	return oldSchema, newSchema
}

func indexByName(d *schemaDiff) map[string]*messageDiff {
	byName := make(map[string]*messageDiff)
	for _, md := range d.messages {
		byName[md.fullname] = md
	}
	return byName
}

func fieldByName(md *messageDiff, name string) *fieldDiff {
	for _, fd := range md.fields {
		if fd.name == name {
			return fd
		}
	}
	return nil
}

// the two fixed schemas contain nested messages (shop.v1.Order.Item),
// cross-package references (common.v1.Address / common.v1.Money),
// a circular reference (Order <-> Customer) and two messages that share
// a simple name across packages (shop.v1.Address vs common.v1.Address).
func TestCollectSchema(t *testing.T) {
	oldSchema, _ := collectBoth(t)

	for _, fullname := range []string{
		"common.v1.Status",
		"common.v1.Address",
		"common.v1.Money",
		"shop.v1.Address",
		"shop.v1.Order",
		"shop.v1.Order.Item", // nested message
		"shop.v1.Customer",
	} {
		if _, found := oldSchema.messages[fullname]; !found {
			t.Errorf("expected message %s to be collected", fullname)
		}
	}

	// declaration order is preserved
	if got := oldSchema.order[0]; got != "common.v1.Status" {
		t.Errorf("expected the first declared type to be common.v1.Status, got %s", got)
	}

	order := oldSchema.messages["shop.v1.Order"]
	fields := make(map[string]*schemaField)
	for _, f := range order.fields {
		fields[f.name] = f
	}

	// cross-package reference, fully qualified in the source
	if got := fields["shipping"].resolved; got != "common.v1.Address" {
		t.Errorf("shipping: expected resolution to common.v1.Address, got %q", got)
	}
	// same simple name, different package: must resolve to shop.v1.Address
	if got := fields["billing"].resolved; got != "shop.v1.Address" {
		t.Errorf("billing: expected resolution to shop.v1.Address (same package wins), got %q", got)
	}
	if got := fields["billing"].kind; got != Message {
		t.Errorf("billing: expected kind Message, got %v", got)
	}

	// circular reference Order <-> Customer: both edges exist, collection terminates
	haveEdge := func(from, to string) bool {
		for _, e := range oldSchema.edges {
			if e.from == from && e.to == to {
				return true
			}
		}
		return false
	}
	if !haveEdge("shop.v1.Order", "shop.v1.Customer") || !haveEdge("shop.v1.Customer", "shop.v1.Order") {
		t.Errorf("expected both edges of the Order <-> Customer cycle")
	}
	if !haveEdge("shop.v1.Order.Item", "common.v1.Money") {
		t.Errorf("expected an edge from the nested message to common.v1.Money")
	}
}

func TestDiffDetectsChanges(t *testing.T) {
	oldSchema, newSchema := collectBoth(t)
	d := diffSchemas(oldSchema, newSchema)
	byName := indexByName(d)

	expectKind := func(fullname string, kind changeKind) {
		t.Helper()
		md, found := byName[fullname]
		if !found {
			t.Errorf("no diff entry for %s", fullname)
			return
		}
		if md.kind != kind {
			t.Errorf("%s: expected kind %v, got %v", fullname, kind, md.kind)
		}
	}

	expectKind("common.v1.Geo", changeAdded)
	expectKind("shop.v1.Legacy", changeRemoved)
	expectKind("common.v1.Money", changeNone)
	expectKind("shop.v1.Customer", changeNone)
	expectKind("shop.v1.Unrelated", changeNone)

	// field type change inside a nested message: int32 -> int64
	expectKind("shop.v1.Order.Item", changeChanged)
	item := byName["shop.v1.Order.Item"]
	quantity := fieldByName(item, "quantity")
	if quantity == nil || quantity.kind != changeChanged {
		t.Fatalf("quantity: expected a changed field, got %+v", quantity)
	}
	if quantity.old.typ != "int32" || quantity.new.typ != "int64" {
		t.Errorf("quantity: expected int32 -> int64, got %s -> %s", quantity.old.typ, quantity.new.typ)
	}

	// field type change in a message living in an imported (cross-package) file
	expectKind("common.v1.Address", changeChanged)
	zip := fieldByName(byName["common.v1.Address"], "zip")
	if zip == nil || zip.kind != changeChanged || zip.old.typ != "int32" || zip.new.typ != "string" {
		t.Errorf("zip: expected int32 -> string, got %+v", zip)
	}

	// field removed + field added
	expectKind("shop.v1.Order", changeChanged)
	order := byName["shop.v1.Order"]
	if fd := fieldByName(order, "legacy_note"); fd == nil || fd.kind != changeRemoved {
		t.Errorf("legacy_note: expected removed, got %+v", fd)
	}
	if fd := fieldByName(order, "region"); fd == nil || fd.kind != changeAdded {
		t.Errorf("region: expected added, got %+v", fd)
	}

	// enum value added
	expectKind("common.v1.Status", changeChanged)
	statusEnum := byName["common.v1.Status"]
	if len(statusEnum.enumAdded) != 1 || statusEnum.enumAdded[0] != "FAILED" {
		t.Errorf("Status: expected added value FAILED, got %v", statusEnum.enumAdded)
	}

	// rename detection: RenameMe -> Renamed (identical shape, same package)
	if len(d.renames) != 1 || d.renames[0].from != "shop.v1.RenameMe" || d.renames[0].to != "shop.v1.Renamed" {
		t.Errorf("expected rename shop.v1.RenameMe -> shop.v1.Renamed, got %+v", d.renames)
	}
	expectKind("shop.v1.RenameMe", changeRenamed)
	expectKind("shop.v1.Renamed", changeRenamed)

	// dependency edges: added edge to the new cross-package type,
	// removed edges of the removed message, unchanged cycle edges
	var addedGeo, removedLegacy, keptCycle int
	for _, ed := range d.edges {
		switch {
		case ed.kind == changeAdded && ed.edge.from == "shop.v1.Address" && ed.edge.to == "common.v1.Geo" && ed.edge.field == "geo":
			addedGeo++
		case ed.kind == changeRemoved && ed.edge.from == "shop.v1.Legacy":
			removedLegacy++
		case ed.kind == changeNone && ed.edge.from == "shop.v1.Customer" && ed.edge.to == "shop.v1.Order":
			keptCycle++
		}
	}
	if addedGeo != 1 {
		t.Errorf("expected one added edge shop.v1.Address -> common.v1.Geo")
	}
	if removedLegacy == 0 {
		t.Errorf("expected removed edges from shop.v1.Legacy")
	}
	if keptCycle != 1 {
		t.Errorf("expected the Customer -> Order edge to survive unchanged")
	}
}

func TestDiffRenderingIsDeterministicAndStable(t *testing.T) {
	oldSchema, newSchema := collectBoth(t)
	d := diffSchemas(oldSchema, newSchema)

	first := renderDiffDOT(d, false)
	second := renderDiffDOT(diffSchemas(oldSchema, newSchema), false)
	if first != second {
		t.Fatalf("rendering is not deterministic")
	}

	// unchanged nodes keep a stable ID derived from the fully qualified name
	customerID := nodeID("shop.v1.Customer")
	if !strings.Contains(first, customerID+" [shape=plaintext") {
		t.Errorf("expected the unchanged node to appear with its stable ID %s", customerID)
	}

	// unchanged nodes keep the old declaration order:
	// common.v1.Money is declared before shop.v1.Customer which is before shop.v1.Unrelated
	moneyAt := strings.Index(first, nodeID("common.v1.Money")+" [")
	customerAt := strings.Index(first, customerID+" [")
	unrelatedAt := strings.Index(first, nodeID("shop.v1.Unrelated")+" [")
	if moneyAt < 0 || customerAt < 0 || unrelatedAt < 0 {
		t.Fatalf("expected all unchanged nodes in the output")
	}
	if !(moneyAt < customerAt && customerAt < unrelatedAt) {
		t.Errorf("unchanged nodes are not in the old declaration order")
	}

	// changed nodes are marked, and the type change is visible
	if !strings.Contains(first, "int32 -&gt; int64") {
		t.Errorf("expected the quantity type change to be rendered")
	}
	if !strings.Contains(first, "(changed)") || !strings.Contains(first, "(added)") || !strings.Contains(first, "(removed)") {
		t.Errorf("expected added/removed/changed markers in the output")
	}
}

func TestDiffAffectedOnlyFilter(t *testing.T) {
	oldSchema, newSchema := collectBoth(t)
	d := diffSchemas(oldSchema, newSchema)

	full := renderDiffDOT(d, false)
	filtered := renderDiffDOT(d, true)

	unrelated := nodeID("shop.v1.Unrelated")
	if !strings.Contains(full, unrelated) {
		t.Fatalf("expected the unaffected node in the full output")
	}
	if strings.Contains(filtered, unrelated) {
		t.Errorf("the unaffected, unconnected node should be filtered out")
	}

	// the changed nested message stays, and so do its dependency (Money)
	// and its transitive dependents (Order, Customer)
	for _, fullname := range []string{"shop.v1.Order.Item", "common.v1.Money", "shop.v1.Order", "shop.v1.Customer", "common.v1.Geo"} {
		if !strings.Contains(filtered, nodeID(fullname)) {
			t.Errorf("expected %s to be part of the affected subgraph", fullname)
		}
	}
}

func TestDiffMissingAndIncompatibleInput(t *testing.T) {
	// missing root descriptor: a clear error, no panic
	if _, err := collectSchema("testdata/diff/no-such-file.proto"); err == nil ||
		!strings.Contains(err.Error(), "descriptor not found") {
		t.Errorf("expected a 'descriptor not found' error, got %v", err)
	}

	// unparseable (incompatible) descriptor: a clear error, no panic
	if _, err := collectSchema("testdata/diff/broken/bad.proto"); err == nil ||
		!strings.Contains(err.Error(), "incompatible descriptor") {
		t.Errorf("expected an 'incompatible descriptor' error, got %v", err)
	}

	// a missing import is recorded and reported, collection continues
	s, err := collectSchema("testdata/diff/missingref/main.proto")
	if err != nil {
		t.Fatalf("a missing import should not fail the collection: %v", err)
	}
	if len(s.missing) != 1 || s.missing[0] != "nonexistent.proto" {
		t.Errorf("expected nonexistent.proto to be reported as missing, got %v", s.missing)
	}
	if _, found := s.messages["lost.v1.Orphan"]; !found {
		t.Errorf("expected the rest of the schema to be collected despite the missing import")
	}

	// syntax change between the versions is surfaced
	oldSyntax, err := collectSchema("testdata/diff/syntax/old.proto")
	if err != nil {
		t.Fatal(err)
	}
	newSyntax, err := collectSchema("testdata/diff/syntax/new.proto")
	if err != nil {
		t.Fatal(err)
	}
	d := diffSchemas(oldSyntax, newSyntax)
	if d.syntaxOld != "proto3" || d.syntaxNew != "proto2" {
		t.Errorf("expected syntax change proto3 -> proto2, got %s -> %s", d.syntaxOld, d.syntaxNew)
	}
	if !strings.Contains(renderDiffDOT(d, false), "incompatible syntax change") {
		t.Errorf("expected the syntax change warning in the DOT output")
	}
}

//----------------------------------------------------------------------------------------------------------------------
// the original single-version generation must keep working unchanged

func setupGeneration(t *testing.T) {
	t.Helper()
	*g_generated = t.TempDir()
	*g_output = ""
	if g_config == nil {
		config, err := support.LoadConfig("config.json", false)
		if err != nil {
			t.Fatalf("failed to load config.json: %v", err)
		}
		g_config = config
		templates := config["templates"].(map[string]interface{})
		tmplDir, err := support.GetLocation(config, "templates")
		if err != nil {
			t.Fatalf("failed to locate templates: %v", err)
		}
		if err := plus.PreloadTemplates(templates, templFuncs, tmplDir); err != nil {
			t.Fatalf("failed to preload templates: %v", err)
		}
	}
}

func TestSingleVersionGeneration(t *testing.T) {
	setupGeneration(t)

	pbs := NewPbs()
	if !process(pbs, oldSchemaRoot, "") {
		t.Fatalf("failed to process %s", oldSchemaRoot)
	}

	data, err := os.ReadFile(pbs.outputFile)
	if err != nil {
		t.Fatalf("failed to read the generated file: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"digraph protodot",
		"Order",       // message
		"Item",        // nested message
		"Customer",    // circular reference partner
		"Address",     // same simple name in two packages
		"Money",       // cross-package reference target
		"legacy_note", // a plain field
		"quantity",    // the field whose type changes in the new schema
		"int32",       // ... with its original type
		"cluster_",    // imported file wrapped into a cluster
		"->",          // dependency edges
	} {
		if !strings.Contains(content, want) {
			t.Errorf("expected the generated .dot to contain %q", want)
		}
	}
}
