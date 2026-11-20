// Copyright 2017 Seamia Corporation. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/emicklei/proto"
	"github.com/seamia/tools/support"
)

//----------------------------------------------------------------------------------------------------------------------
// schema diff: model
//
// A self-contained descriptor comparison pipeline (collect -> diff -> render).
// It deliberately does NOT reuse the single-version rendering state (pbstate),
// so the original single-version output is left completely untouched.

type schemaField struct {
	name     string
	typ      string // the type as written in the source
	resolved string // fully qualified name of the target type ("" for simple/unresolved types)
	kind     Kind
	ordinal  int
	repeated bool
	mapKey   string // non-empty for map fields (key type)
	oneof    string // name of the enclosing oneof ("" when not in one)
}

func (f *schemaField) displayType() string {
	if len(f.mapKey) > 0 {
		return "map<" + f.mapKey + ", " + f.typ + ">"
	}
	return f.typ
}

// typeIdentity is what gets compared to decide whether a field's type has changed.
// When both sides resolve to a concrete type we compare the fully qualified names
// (so "同名不同包" is handled correctly); otherwise we fall back to the type as written.
func (f *schemaField) typeIdentity() string {
	if len(f.resolved) > 0 {
		return "resolved:" + f.resolved
	}
	return "written:" + f.typ
}

type schemaMessage struct {
	fullname string // fully qualified name, e.g. "shop.v1.Order.Item"
	name     string // short name, e.g. "Item"
	pkg      string
	file     string
	isEnum   bool
	enumVals []string
	fields   []*schemaField
}

type schemaEdge struct {
	from  string // fully qualified name of the source message
	to    string // fully qualified name of the target type
	field string // name of the field carrying the reference
}

type schema struct {
	root       string
	rootSyntax string
	messages   map[string]*schemaMessage
	order      []string // fully qualified names, in declaration order
	edges      []*schemaEdge
	files      []string
	missing    []string // imports that could not be located
}

//----------------------------------------------------------------------------------------------------------------------
// schema diff: collection

type schemaCollector struct {
	s       *schema
	rootDir string
	seen    map[string]bool
}

func collectSchema(root string) (*schema, error) {
	if !Exists(root) {
		return nil, fmt.Errorf("descriptor not found: %s", root)
	}
	rootDir, _ := pathSplit(root)
	collector := &schemaCollector{
		s: &schema{
			root:     root,
			messages: make(map[string]*schemaMessage),
		},
		rootDir: rootDir,
		seen:    make(map[string]bool),
	}
	if err := collector.collectFile(root, true); err != nil {
		return nil, err
	}
	collector.resolveAll()
	return collector.s, nil
}

func joinScope(scope, name string) string {
	if len(scope) == 0 {
		return name
	}
	return scope + "." + name
}

func (c *schemaCollector) collectFile(name string, isRoot bool) error {
	if c.seen[name] {
		return nil
	}
	c.seen[name] = true

	reader, err := Find(name, c.rootDir)
	if err != nil {
		if isRoot {
			return fmt.Errorf("cannot open descriptor [%s]: %v", name, err)
		}
		// a missing import is not fatal: it is recorded and reported instead
		c.s.missing = append(c.s.missing, name)
		return nil
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	parser := proto.NewParser(reader)
	definition, err := parser.Parse()
	if err != nil {
		return fmt.Errorf("incompatible descriptor [%s]: %v", name, err)
	}
	if definition == nil {
		return fmt.Errorf("incompatible descriptor [%s]: nothing could be parsed", name)
	}
	c.s.files = append(c.s.files, name)

	pkg := ""
	for _, element := range definition.Elements {
		switch actual := element.(type) {
		case *proto.Syntax:
			if isRoot {
				c.s.rootSyntax = actual.Value
			}
		case *proto.Package:
			pkg = actual.Name
		case *proto.Import:
			if err := c.collectFile(actual.Filename, false); err != nil {
				return err
			}
		case *proto.Message:
			c.collectMessage(actual, pkg, pkg, name)
		case *proto.Enum:
			c.collectEnum(actual, pkg, pkg, name)
		}
	}
	return nil
}

func (c *schemaCollector) collectMessage(msg *proto.Message, scope, pkg, file string) {
	if msg.IsExtend {
		return
	}
	fullname := joinScope(scope, msg.Name)
	sm := &schemaMessage{
		fullname: fullname,
		name:     msg.Name,
		pkg:      pkg,
		file:     file,
	}
	c.s.messages[fullname] = sm
	c.s.order = append(c.s.order, fullname)

	for _, element := range msg.Elements {
		switch actual := element.(type) {
		case *proto.NormalField:
			sm.fields = append(sm.fields, &schemaField{
				name:     actual.Name,
				typ:      actual.Type,
				ordinal:  actual.Sequence,
				repeated: actual.Repeated,
			})
		case *proto.MapField:
			sm.fields = append(sm.fields, &schemaField{
				name:    actual.Name,
				typ:     actual.Type,
				mapKey:  actual.KeyType,
				ordinal: actual.Sequence,
			})
		case *proto.Oneof:
			for _, one := range actual.Elements {
				if field, ok := one.(*proto.OneOfField); ok {
					sm.fields = append(sm.fields, &schemaField{
						name:    field.Name,
						typ:     field.Type,
						ordinal: field.Sequence,
						oneof:   actual.Name,
					})
				}
			}
		case *proto.Message:
			c.collectMessage(actual, fullname, pkg, file)
		case *proto.Enum:
			c.collectEnum(actual, fullname, pkg, file)
		}
	}
}

func (c *schemaCollector) collectEnum(e *proto.Enum, scope, pkg, file string) {
	fullname := joinScope(scope, e.Name)
	sm := &schemaMessage{
		fullname: fullname,
		name:     e.Name,
		pkg:      pkg,
		file:     file,
		isEnum:   true,
	}
	for _, element := range e.Elements {
		if field, ok := element.(*proto.EnumField); ok {
			sm.enumVals = append(sm.enumVals, field.Name)
		}
	}
	c.s.messages[fullname] = sm
	c.s.order = append(c.s.order, fullname)
}

// resolve implements the protobuf scoping rules: a type reference is looked up
// starting from the innermost scope, walking outward one component at a time.
func (s *schema) resolve(scope, ref string) (string, Kind) {
	if isSimpleType(ref) {
		return "", Simple
	}
	if strings.HasPrefix(ref, ".") {
		candidate := ref[1:]
		if _, found := s.messages[candidate]; found {
			return candidate, s.kindOf(candidate)
		}
		return "", Missing
	}
	prefix := scope
	for {
		candidate := ref
		if len(prefix) > 0 {
			candidate = prefix + "." + ref
		}
		if _, found := s.messages[candidate]; found {
			return candidate, s.kindOf(candidate)
		}
		if len(prefix) == 0 {
			break
		}
		if index := strings.LastIndex(prefix, "."); index >= 0 {
			prefix = prefix[:index]
		} else {
			prefix = ""
		}
	}
	return "", Missing
}

func (s *schema) kindOf(fullname string) Kind {
	if msg, found := s.messages[fullname]; found && msg.isEnum {
		return Enum
	}
	return Message
}

// resolveAll runs the second pass: with every declaration known, field types
// are resolved and the dependency edges are built (in declaration order).
func (c *schemaCollector) resolveAll() {
	for _, fullname := range c.s.order {
		sm := c.s.messages[fullname]
		if sm.isEnum {
			continue
		}
		for _, field := range sm.fields {
			resolved, kind := c.s.resolve(sm.fullname, field.typ)
			field.resolved = resolved
			field.kind = kind
			if kind == Message || kind == Enum {
				c.s.edges = append(c.s.edges, &schemaEdge{
					from:  sm.fullname,
					to:    resolved,
					field: field.name,
				})
			}
		}
	}
}

//----------------------------------------------------------------------------------------------------------------------
// schema diff: comparison

type changeKind int

const (
	changeNone changeKind = iota
	changeAdded
	changeRemoved
	changeChanged
	changeRenamed
)

type fieldDiff struct {
	name string
	kind changeKind
	old  *schemaField
	new  *schemaField
}

type messageDiff struct {
	fullname string // for renamed entries: the name on their own side
	kind     changeKind
	old      *schemaMessage
	new      *schemaMessage

	fields      []*fieldDiff
	enumAdded   []string
	enumRemoved []string
	renamedTo   string // for changeRenamed: the fullname on the other side
}

type edgeDiff struct {
	edge *schemaEdge
	kind changeKind
}

type renamePair struct {
	from string
	to   string
}

type schemaDiff struct {
	old *schema
	new *schema

	messages []*messageDiff
	edges    []*edgeDiff
	renames  []renamePair

	syntaxOld string
	syntaxNew string
}

func diffSchemas(oldSchema, newSchema *schema) *schemaDiff {
	d := &schemaDiff{
		old:       oldSchema,
		new:       newSchema,
		syntaxOld: oldSchema.rootSyntax,
		syntaxNew: newSchema.rootSyntax,
	}

	// entries present in the old schema keep the old declaration order
	for _, fullname := range oldSchema.order {
		oldMsg := oldSchema.messages[fullname]
		if newMsg, found := newSchema.messages[fullname]; found {
			d.messages = append(d.messages, diffMessage(oldMsg, newMsg))
		} else {
			md := &messageDiff{fullname: fullname, kind: changeRemoved, old: oldMsg}
			for _, field := range oldMsg.fields {
				md.fields = append(md.fields, &fieldDiff{name: field.name, kind: changeRemoved, old: field})
			}
			d.messages = append(d.messages, md)
		}
	}
	// entries that only exist in the new schema follow, in the new declaration order
	for _, fullname := range newSchema.order {
		if _, found := oldSchema.messages[fullname]; !found {
			newMsg := newSchema.messages[fullname]
			md := &messageDiff{fullname: fullname, kind: changeAdded, new: newMsg}
			for _, field := range newMsg.fields {
				md.fields = append(md.fields, &fieldDiff{name: field.name, kind: changeAdded, new: field})
			}
			d.messages = append(d.messages, md)
		}
	}

	d.detectRenames()
	d.diffEdges()
	return d
}

func diffMessage(oldMsg, newMsg *schemaMessage) *messageDiff {
	md := &messageDiff{
		fullname: oldMsg.fullname,
		kind:     changeNone,
		old:      oldMsg,
		new:      newMsg,
	}

	if oldMsg.isEnum != newMsg.isEnum {
		md.kind = changeChanged
		return md
	}

	if oldMsg.isEnum {
		inNew := make(map[string]bool)
		for _, value := range newMsg.enumVals {
			inNew[value] = true
		}
		inOld := make(map[string]bool)
		for _, value := range oldMsg.enumVals {
			inOld[value] = true
			if !inNew[value] {
				md.enumRemoved = append(md.enumRemoved, value)
			}
		}
		for _, value := range newMsg.enumVals {
			if !inOld[value] {
				md.enumAdded = append(md.enumAdded, value)
			}
		}
		if len(md.enumAdded) > 0 || len(md.enumRemoved) > 0 {
			md.kind = changeChanged
		}
		return md
	}

	newByName := make(map[string]*schemaField)
	for _, field := range newMsg.fields {
		newByName[field.name] = field
	}
	oldByName := make(map[string]*schemaField)
	for _, field := range oldMsg.fields {
		oldByName[field.name] = field
	}

	for _, oldField := range oldMsg.fields {
		if newField, found := newByName[oldField.name]; found {
			switch {
			case oldField.typeIdentity() != newField.typeIdentity(),
				oldField.ordinal != newField.ordinal,
				oldField.repeated != newField.repeated,
				oldField.mapKey != newField.mapKey:
				md.fields = append(md.fields, &fieldDiff{name: oldField.name, kind: changeChanged, old: oldField, new: newField})
			default:
				md.fields = append(md.fields, &fieldDiff{name: oldField.name, kind: changeNone, old: oldField, new: newField})
			}
		} else {
			md.fields = append(md.fields, &fieldDiff{name: oldField.name, kind: changeRemoved, old: oldField})
		}
	}
	for _, newField := range newMsg.fields {
		if _, found := oldByName[newField.name]; !found {
			md.fields = append(md.fields, &fieldDiff{name: newField.name, kind: changeAdded, new: newField})
		}
	}

	for _, field := range md.fields {
		if field.kind != changeNone {
			md.kind = changeChanged
			break
		}
	}
	return md
}

// fieldSignature identifies a message "shape"; a rename keeps the shape but
// changes the name. Types are compared as written so that a rename of the
// message itself does not break its own signature.
func fieldSignature(msg *schemaMessage) string {
	parts := make([]string, 0, len(msg.fields)+2)
	parts = append(parts, msg.pkg, strconv.FormatBool(msg.isEnum))
	for _, field := range msg.fields {
		parts = append(parts, fmt.Sprintf("%s|%s|%d|%v|%s|%s",
			field.name, field.typ, field.ordinal, field.repeated, field.mapKey, field.oneof))
	}
	for _, value := range msg.enumVals {
		parts = append(parts, "enum:"+value)
	}
	return strings.Join(parts, ";")
}

func (d *schemaDiff) detectRenames() {
	added := make([]*messageDiff, 0)
	for _, md := range d.messages {
		if md.kind == changeAdded {
			added = append(added, md)
		}
	}
	used := make(map[*messageDiff]bool)

	for _, removed := range d.messages {
		if removed.kind != changeRemoved {
			continue
		}
		signature := fieldSignature(removed.old)
		for _, candidate := range added {
			if used[candidate] {
				continue
			}
			if fieldSignature(candidate.new) == signature {
				removed.kind = changeRenamed
				removed.renamedTo = candidate.fullname
				candidate.kind = changeRenamed
				candidate.renamedTo = removed.fullname
				used[candidate] = true
				d.renames = append(d.renames, renamePair{from: removed.fullname, to: candidate.fullname})
				break
			}
		}
	}
}

func edgeKey(edge *schemaEdge) string {
	return edge.from + "\x00" + edge.to + "\x00" + edge.field
}

func (d *schemaDiff) diffEdges() {
	inNew := make(map[string]bool)
	for _, edge := range d.new.edges {
		inNew[edgeKey(edge)] = true
	}
	inOld := make(map[string]bool)
	for _, edge := range d.old.edges {
		inOld[edgeKey(edge)] = true
		if inNew[edgeKey(edge)] {
			d.edges = append(d.edges, &edgeDiff{edge: edge, kind: changeNone})
		} else {
			d.edges = append(d.edges, &edgeDiff{edge: edge, kind: changeRemoved})
		}
	}
	for _, edge := range d.new.edges {
		if !inOld[edgeKey(edge)] {
			d.edges = append(d.edges, &edgeDiff{edge: edge, kind: changeAdded})
		}
	}
}

// affected computes the set of messages that make up the "affected dependency
// subgraph": every changed/added/removed/renamed node, everything those nodes
// directly depend on, and everything that (transitively) depends on them.
// Cycles are handled by the visited set.
func (d *schemaDiff) affected() map[string]bool {
	affected := make(map[string]bool)
	for _, md := range d.messages {
		if md.kind != changeNone {
			affected[md.fullname] = true
			if md.kind == changeRenamed && len(md.renamedTo) > 0 {
				affected[md.renamedTo] = true
			}
		}
	}

	allEdges := make([]*schemaEdge, 0, len(d.old.edges)+len(d.new.edges))
	allEdges = append(allEdges, d.old.edges...)
	allEdges = append(allEdges, d.new.edges...)

	// direct dependencies of the affected nodes
	for _, edge := range allEdges {
		if affected[edge.from] {
			affected[edge.to] = true
		}
	}

	// transitive dependents (who references an affected node, directly or not)
	reverse := make(map[string][]string)
	for _, edge := range allEdges {
		reverse[edge.to] = append(reverse[edge.to], edge.from)
	}
	queue := make([]string, 0, len(affected))
	for name := range affected {
		queue = append(queue, name)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[current] {
			if !affected[dependent] {
				affected[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return affected
}

func (d *schemaDiff) summary() string {
	counts := make(map[changeKind]int)
	for _, md := range d.messages {
		counts[md.kind]++
	}
	edgesAdded, edgesRemoved := 0, 0
	for _, edge := range d.edges {
		switch edge.kind {
		case changeAdded:
			edgesAdded++
		case changeRemoved:
			edgesRemoved++
		}
	}
	return fmt.Sprintf("diff summary: messages: %d added, %d removed, %d changed, %d renamed, %d unchanged; edges: %d added, %d removed",
		counts[changeAdded], counts[changeRemoved], counts[changeChanged], counts[changeRenamed]/2, counts[changeNone],
		edgesAdded, edgesRemoved)
}

//----------------------------------------------------------------------------------------------------------------------
// schema diff: DOT rendering

// nodeID is a stable node identifier: it depends only on the fully qualified
// name, so an unchanged node keeps the same ID across versions and runs.
func nodeID(fullname string) string {
	return "Node_" + support.NameToId(fullname, 16)
}

var diffStatusColors = map[changeKind][2]string{ // border color, header background
	changeNone:    {"gray60", "gray90"},
	changeAdded:   {"green4", "palegreen"},
	changeRemoved: {"red4", "mistyrose"},
	changeChanged: {"darkorange", "lightgoldenrod"},
	changeRenamed: {"blue4", "lightblue"},
}

var diffStatusTags = map[changeKind]string{
	changeAdded:   " (added)",
	changeRemoved: " (removed)",
	changeChanged: " (changed)",
	changeRenamed: " (renamed)",
}

func escHTML(text string) string {
	text = strings.Replace(text, "&", "&amp;", -1)
	text = strings.Replace(text, "<", "&lt;", -1)
	text = strings.Replace(text, ">", "&gt;", -1)
	return text
}

func renderDiffDOT(d *schemaDiff, affectedOnly bool) string {
	var keep map[string]bool
	if affectedOnly {
		keep = d.affected()
	}
	visible := func(fullname string) bool {
		return keep == nil || keep[fullname]
	}

	var b strings.Builder
	b.WriteString("/*\n\tdo not edit:\n\tauto-generated by github.com/seamia/protodot (diff mode)\n*/\n")
	b.WriteString("digraph protodot_diff {\n\n")
	fmt.Fprintf(&b, "\t/* old:   %s */\n", d.old.root)
	fmt.Fprintf(&b, "\t/* new:   %s */\n", d.new.root)
	fmt.Fprintf(&b, "\t/* %s */\n", d.summary())
	for _, pair := range d.renames {
		fmt.Fprintf(&b, "\t/* rename detected: %s -> %s */\n", pair.from, pair.to)
	}
	if d.syntaxOld != d.syntaxNew {
		fmt.Fprintf(&b, "\t/* warning: incompatible syntax change: %s -> %s */\n", d.syntaxOld, d.syntaxNew)
	}
	for _, name := range d.old.missing {
		fmt.Fprintf(&b, "\t/* missing import (old): %s */\n", name)
	}
	for _, name := range d.new.missing {
		fmt.Fprintf(&b, "\t/* missing import (new): %s */\n", name)
	}
	if affectedOnly {
		b.WriteString("\t/* output limited to the affected dependency subgraph */\n")
	}

	fmt.Fprintf(&b, "\n\trankdir=LR;\n\tlabel=\"schema diff: %s -> %s\";\n", d.old.root, d.new.root)
	b.WriteString("\ttooltip=\"schema diff\";\n")
	b.WriteString("\tbgcolor=\"transparent\"\n\n")
	b.WriteString("\tnode [\n\t\tshape=plaintext\n\t\tfontsize=10\n\t];\n\n")

	// legend
	b.WriteString("\tsubgraph cluster_legend {\n")
	b.WriteString("\t\tlabel=\"legend\";\n")
	b.WriteString("\t\tstyle=\"rounded\";\n")
	b.WriteString("\t\tlegend_added [shape=box style=filled fillcolor=palegreen label=\"added\"];\n")
	b.WriteString("\t\tlegend_removed [shape=box style=filled fillcolor=mistyrose label=\"removed\"];\n")
	b.WriteString("\t\tlegend_changed [shape=box style=filled fillcolor=lightgoldenrod label=\"changed\"];\n")
	b.WriteString("\t\tlegend_renamed [shape=box style=filled fillcolor=lightblue label=\"renamed\"];\n")
	b.WriteString("\t}\n\n")

	b.WriteString("\t/* ------ nodes ------ */\n")
	for _, md := range d.messages {
		if !visible(md.fullname) {
			continue
		}
		renderDiffNode(&b, md)
	}

	b.WriteString("\n\t/* ------ connections ------ */\n")
	for _, ed := range d.edges {
		if !visible(ed.edge.from) || !visible(ed.edge.to) {
			continue
		}
		renderDiffEdge(&b, ed)
	}

	for _, pair := range d.renames {
		if visible(pair.from) && visible(pair.to) {
			fmt.Fprintf(&b, "\t%s -> %s [color=\"blue4\" style=dotted constraint=false label=\"renamed\" tooltip=\"renamed: %s -> %s\"];\n",
				nodeID(pair.from), nodeID(pair.to), pair.from, pair.to)
		}
	}

	b.WriteString("}\n")
	return b.String()
}

func renderDiffNode(b *strings.Builder, md *messageDiff) {
	msg := md.new
	if msg == nil {
		msg = md.old
	}
	colors := diffStatusColors[md.kind]

	fmt.Fprintf(b, "\t%s [shape=plaintext tooltip=\"%s\" label=<\n", nodeID(md.fullname), md.fullname)
	fmt.Fprintf(b, "<TABLE BORDER=\"1\" CELLBORDER=\"0\" CELLSPACING=\"0\" COLOR=\"%s\" BGCOLOR=\"white\">\n", colors[0])
	fmt.Fprintf(b, "\t<TR><TD COLSPAN=\"3\" PORT=\"header\" BGCOLOR=\"%s\" ALIGN=\"left\"><b>%s</b>%s</TD></TR>\n",
		colors[1], escHTML(msg.name), diffStatusTags[md.kind])

	if msg.isEnum {
		removed := make(map[string]bool)
		for _, value := range md.enumRemoved {
			removed[value] = true
		}
		added := make(map[string]bool)
		for _, value := range md.enumAdded {
			added[value] = true
		}
		values := msg.enumVals
		if md.old != nil {
			values = md.old.enumVals
		}
		seen := make(map[string]bool)
		for _, value := range values {
			seen[value] = true
			bg := ""
			if removed[value] {
				bg = " BGCOLOR=\"mistyrose\""
			}
			fmt.Fprintf(b, "\t<TR><TD%s></TD><TD%s ALIGN=\"left\">%s</TD><TD%s></TD></TR>\n", bg, bg, escHTML(value), bg)
		}
		if md.new != nil {
			for _, value := range md.new.enumVals {
				if seen[value] || !added[value] {
					continue
				}
				fmt.Fprintf(b, "\t<TR><TD BGCOLOR=\"palegreen\"></TD><TD BGCOLOR=\"palegreen\" ALIGN=\"left\">%s</TD><TD BGCOLOR=\"palegreen\"></TD></TR>\n", escHTML(value))
			}
		}
	} else {
		for _, field := range md.fields {
			renderDiffField(b, field)
		}
	}

	b.WriteString("</TABLE>>];\n")
}

func renderDiffField(b *strings.Builder, fd *fieldDiff) {
	field := fd.new
	if field == nil {
		field = fd.old
	}

	bg := ""
	ordinal := strconv.Itoa(field.ordinal)
	typ := escHTML(field.displayType())

	switch fd.kind {
	case changeAdded:
		bg = "palegreen"
	case changeRemoved:
		bg = "mistyrose"
	case changeChanged:
		bg = "orange"
		if fd.old.typeIdentity() != fd.new.typeIdentity() {
			typ = escHTML(fd.old.displayType()) + " -&gt; " + escHTML(fd.new.displayType())
		}
		if fd.old.ordinal != fd.new.ordinal {
			ordinal = strconv.Itoa(fd.old.ordinal) + "-&gt;" + strconv.Itoa(fd.new.ordinal)
		}
	}

	bgAttr := ""
	if len(bg) > 0 {
		bgAttr = " BGCOLOR=\"" + bg + "\""
	}
	repeated := ""
	if field.repeated {
		repeated = "[...] "
	}
	fmt.Fprintf(b, "\t<TR><TD%s ALIGN=\"right\">%s%s</TD><TD%s ALIGN=\"left\">%s</TD><TD%s PORT=\"po%s\" ALIGN=\"right\"><i>%s</i></TD></TR>\n",
		bgAttr, repeated, ordinal, bgAttr, escHTML(field.name), bgAttr, field.name, typ)
}

func renderDiffEdge(b *strings.Builder, ed *edgeDiff) {
	edge := ed.edge
	attrs := "color=\"gray40\""
	switch ed.kind {
	case changeAdded:
		attrs = "color=\"green4\" penwidth=2"
	case changeRemoved:
		attrs = "color=\"red4\" style=dashed"
	}
	fmt.Fprintf(b, "\t%s:po%s:e -> %s:header [%s tooltip=\"%s.%s -> %s\"];\n",
		nodeID(edge.from), edge.field, nodeID(edge.to), attrs, edge.from, edge.field, edge.to)
}

//----------------------------------------------------------------------------------------------------------------------
// schema diff: entry point

func runDiff(oldName, newName string, affectedOnly bool) error {
	status("comparing descriptors:", oldName, "->", newName)

	oldSchema, err := collectSchema(oldName)
	if err != nil {
		return err
	}
	newSchema, err := collectSchema(newName)
	if err != nil {
		return err
	}

	for _, name := range oldSchema.missing {
		status("missing import in the old schema:", name)
	}
	for _, name := range newSchema.missing {
		status("missing import in the new schema:", name)
	}

	diff := diffSchemas(oldSchema, newSchema)

	for _, pair := range diff.renames {
		status("rename detected:", pair.from, "->", pair.to)
	}
	if diff.syntaxOld != diff.syntaxNew {
		status("warning: incompatible syntax change:", diff.syntaxOld, "->", diff.syntaxNew)
	}
	status(diff.summary())

	dot := renderDiffDOT(diff, affectedOnly)

	genDir := getTargetPath()
	outputFileName := getProtoName(oldName+"_vs_"+newName, "diff")
	if len(*g_output) > 0 {
		outputFileName = *g_output
	}
	target := path.Join(genDir, outputFileName+".dot")
	if err := os.WriteFile(target, []byte(dot), 0644); err != nil {
		return err
	}
	status("created file:", target)

	graphviz(target, options(generateSvg), options(generatePng))
	return nil
}
