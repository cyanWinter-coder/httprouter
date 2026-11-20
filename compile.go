// Copyright 2013 Julien Schmidt. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// in the LICENSE file.

package httprouter

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
)

// Rule is a single named route registration, as accepted by Compiler and
// Router.CompileRules. The name identifies the rule in diagnostics, so that
// conflicts found in a merged route table can be attributed to the team that
// contributed the rule.
type Rule struct {
	Name   string
	Method string
	Path   string
	Handle Handle
}

// DiagKind classifies a Diagnostic.
type DiagKind uint8

const (
	// DiagInvalid marks a rule that cannot be registered at all (empty
	// method, malformed path, nil handle, illegal wildcard syntax).
	DiagInvalid DiagKind = iota
	// DiagDuplicate marks rules that register the same method and path
	// more than once.
	DiagDuplicate
	// DiagParamNameConflict marks rules that match the same requests but
	// use different parameter names for a segment, e.g. /user/:id and
	// /user/:name.
	DiagParamNameConflict
	// DiagUnreachableBranch marks a branch that can never be reached
	// because a sibling parameter segment claims the same position, e.g.
	// /user/new next to /user/:id.
	DiagUnreachableBranch
	// DiagCatchAllShadow marks rules that are shadowed by a catch-all
	// parameter registered for a parent path, e.g. /files/readme next to
	// /files/*filepath.
	DiagCatchAllShadow
)

func (k DiagKind) String() string {
	switch k {
	case DiagInvalid:
		return "invalid"
	case DiagDuplicate:
		return "duplicate"
	case DiagParamNameConflict:
		return "param-name-conflict"
	case DiagUnreachableBranch:
		return "unreachable-branch"
	case DiagCatchAllShadow:
		return "catch-all-shadow"
	}
	return "unknown"
}

// Diagnostic describes a single conflict or invalid rule found while
// compiling a rule set. Rules and Paths list the involved rules (names and
// paths, in the same order). Sample is an example request path that
// demonstrates the conflict; parameter segments are rendered as {name}.
type Diagnostic struct {
	Kind    DiagKind
	Method  string
	Rules   []string
	Paths   []string
	Sample  string
	Message string
}

func (d Diagnostic) String() string {
	return d.Message
}

// Diagnostics is the list of conflicts found by a failed compilation.
// It implements the error interface. The list is sorted deterministically
// (by method, sample request, kind and involved rules), so compiling the
// same rule set always reports the conflicts in the same order.
type Diagnostics []Diagnostic

func (ds Diagnostics) Error() string {
	switch len(ds) {
	case 0:
		return "httprouter: no diagnostics"
	case 1:
		return ds[0].Message
	}
	msg := fmt.Sprintf("httprouter: %d route conflicts:", len(ds))
	for _, d := range ds {
		msg += "\n  - " + d.Message
	}
	return msg
}

// Compiler stages a set of named rules and compiles them into a Snapshot in
// one pass, without touching the router's active route table. A Compiler is
// single-use per compilation but rules may be staged incrementally; Close
// abandons the staged rules.
type Compiler struct {
	r      *Router
	rules  []Rule
	closed bool
}

// Compiler returns a Compiler that stages rules for a later atomic Publish
// of the compiled Snapshot on this router.
func (r *Router) Compiler() *Compiler {
	return &Compiler{r: r}
}

// Add stages a single named rule.
func (c *Compiler) Add(name, method, path string, handle Handle) {
	c.AddRule(Rule{Name: name, Method: method, Path: path, Handle: handle})
}

// AddRule stages a single rule.
func (c *Compiler) AddRule(rule Rule) {
	if c.closed {
		panic("httprouter: AddRule on closed Compiler")
	}
	c.rules = append(c.rules, rule)
}

// AddRules stages multiple rules at once.
func (c *Compiler) AddRules(rules ...Rule) {
	for _, rule := range rules {
		c.AddRule(rule)
	}
}

// Compile compiles all staged rules. See Router.CompileRules.
func (c *Compiler) Compile() (*Snapshot, Diagnostics) {
	if c.closed {
		return nil, Diagnostics{{
			Kind:    DiagInvalid,
			Message: "httprouter: Compile on closed Compiler",
		}}
	}
	return c.r.CompileRules(c.rules...)
}

// Close abandons the compilation and discards all staged rules. Nothing was
// or will be published, and no temporary tree nodes are retained: the
// compiler only ever builds detached structures, so dropping them here
// leaves no trace in the router.
func (c *Compiler) Close() {
	c.rules = nil
	c.closed = true
}

// CompileRules compiles a complete rule set into a Snapshot in one pass.
// The router's active route table is never touched: the new trees are built
// detached, and only a successful Snapshot can later be swapped in with
// Publish. If any rule is invalid or conflicts with another rule, compilation
// fails and all independent conflicts are reported at once as Diagnostics.
//
// The router's SaveMatchedRoutePath and HandleOPTIONS options are applied to
// the snapshot at compile time, so lookup, parameter and redirect semantics
// after Publish match those of incrementally registered routes.
func (r *Router) CompileRules(rules ...Rule) (*Snapshot, Diagnostics) {
	if diags := analyzeRules(rules); len(diags) > 0 {
		return nil, diags
	}

	trees := make(map[string]*node)
	var maxParams uint16
	varsCount := uint16(0)
	if r.SaveMatchedRoutePath {
		varsCount++
	}

	var diags Diagnostics
	for i := range rules {
		rule := &rules[i]
		handle := rule.Handle
		if r.SaveMatchedRoutePath {
			handle = r.saveMatchedRoutePath(rule.Path, handle)
		}

		root := trees[rule.Method]
		if root == nil {
			root = new(node)
			trees[rule.Method] = root
		}

		// Safety net: analyzeRules already rejected every conflict addRoute
		// can detect, so this should never fire. If it does, the compile
		// still fails cleanly instead of publishing a half-built table.
		if msg := safeAddRoute(root, rule.Path, handle); msg != "" {
			diags = append(diags, Diagnostic{
				Kind:    DiagInvalid,
				Method:  rule.Method,
				Rules:   []string{ruleName(rule, i)},
				Paths:   []string{rule.Path},
				Sample:  samplePath(rule.Path),
				Message: fmt.Sprintf("httprouter: rule %q (%s %s): %s", ruleName(rule, i), rule.Method, rule.Path, msg),
			})
		}

		if paramsCount := countParams(rule.Path); paramsCount+varsCount > maxParams {
			maxParams = paramsCount + varsCount
		}
	}
	if len(diags) > 0 {
		return nil, sortDiagnostics(diags)
	}

	return &Snapshot{
		trees:         trees,
		maxParams:     maxParams,
		globalAllowed: computeGlobalAllowed(trees, r.HandleOPTIONS),
		hash:          hashRules(rules),
	}, nil
}

func safeAddRoute(root *node, path string, handle Handle) (msg string) {
	defer func() {
		if rcv := recover(); rcv != nil {
			msg = fmt.Sprint(rcv)
		}
	}()
	root.addRoute(path, handle)
	return ""
}

// Snapshot is an immutable, fully compiled route table. A Snapshot is
// created by CompileRules or Compiler.Compile and swapped into a Router with
// Router.Publish. It is safe to use a Snapshot from multiple goroutines.
type Snapshot struct {
	trees         map[string]*node
	maxParams     uint16
	globalAllowed string
	hash          uint64
}

// Lookup allows the manual lookup of a method + path combo against the
// snapshot, with the same semantics as Router.Lookup.
func (s *Snapshot) Lookup(method, path string) (Handle, Params, bool) {
	if root := s.trees[method]; root != nil {
		handle, ps, tsr := root.getValue(path, s.getParams)
		if handle == nil {
			return nil, nil, tsr
		}
		if ps == nil {
			return handle, nil, tsr
		}
		return handle, *ps, tsr
	}
	return nil, nil, false
}

func (s *Snapshot) getParams() *Params {
	ps := make(Params, 0, s.maxParams)
	return &ps
}

// Publish atomically replaces the router's active route table with the
// compiled snapshot. Requests that are already being served finish against
// the old table; new requests use the new one. Readers never block: the swap
// is a single atomic store.
//
// Publishing a snapshot whose content is identical to the active one is
// idempotent: it has no effect and reports false. The return value reports
// whether the route table content changed.
func (r *Router) Publish(s *Snapshot) bool {
	if s == nil {
		panic("httprouter: Publish of nil Snapshot")
	}
	changed := r.loadState().hash != s.hash
	r.state.Store(&routerState{
		trees:         s.trees,
		maxParams:     s.maxParams,
		globalAllowed: s.globalAllowed,
		hash:          s.hash,
	})

	// Lazy-init paramsPool alloc func, mirroring Handle
	if r.paramsPool.New == nil && s.maxParams > 0 {
		r.paramsPool.New = func() interface{} {
			ps := make(Params, 0, r.loadState().maxParams)
			return &ps
		}
	}
	return changed
}

// computeGlobalAllowed replicates the server-wide branch of Router.allowed
// for a detached set of trees.
func computeGlobalAllowed(trees map[string]*node, handleOPTIONS bool) string {
	allowed := make([]string, 0, len(trees)+1)
	for method := range trees {
		if method == http.MethodOptions {
			continue
		}
		allowed = append(allowed, method)
	}
	if len(allowed) == 0 {
		return ""
	}
	if handleOPTIONS {
		allowed = append(allowed, http.MethodOptions)
	}
	sort.Strings(allowed)
	return strings.Join(allowed, ", ")
}

// hashRules computes a content hash over the rule set, independent of the
// order in which the rules were staged.
func hashRules(rules []Rule) uint64 {
	keys := make([]string, 0, len(rules))
	for i := range rules {
		keys = append(keys, rules[i].Method+"\x00"+rules[i].Path+"\x00"+rules[i].Name)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func ruleName(rule *Rule, i int) string {
	if rule.Name != "" {
		return rule.Name
	}
	return fmt.Sprintf("#%d", i)
}

// samplePath renders a route path as a sample request path, replacing
// wildcards with {name} placeholders, e.g. /user/:id becomes /user/{id}.
func samplePath(path string) string {
	var sample string
	for i := 0; i < len(path); i++ {
		if c := path[i]; c == ':' || c == '*' {
			j := i + 1
			for j < len(path) && path[j] != '/' {
				j++
			}
			sample += "{" + path[i+1:j] + "}"
			i = j - 1
			continue
		}
		sample += path[i : i+1]
	}
	return sample
}

// validateRule mirrors the validation performed by Router.Handle and
// node.insertChild, returning a description of the first problem found.
func validateRule(rule *Rule) string {
	if rule.Method == "" {
		return "method must not be empty"
	}
	if len(rule.Path) < 1 || rule.Path[0] != '/' {
		return "path must begin with '/'"
	}
	if rule.Handle == nil {
		return "handle must not be nil"
	}
	path := rule.Path
	for {
		wildcard, i, valid := findWildcard(path)
		if i < 0 {
			return ""
		}
		if !valid {
			return fmt.Sprintf("only one wildcard per path segment is allowed, has: '%s'", wildcard)
		}
		if len(wildcard) < 2 {
			return "wildcards must be named with a non-empty name"
		}
		if wildcard[0] == '*' {
			if i+len(wildcard) != len(path) {
				return "catch-all routes are only allowed at the end of the path"
			}
			if path[i-1] != '/' {
				return "no / before catch-all"
			}
		}
		path = path[i+len(wildcard):]
	}
}

// Segment kinds used by the analytical conflict trie.
const (
	segStatic = iota
	segParam
	segCatchAll
)

// segKind classifies a single path segment. Segments with a wildcard that
// does not start the segment (e.g. "v1:id") are treated as opaque static
// segments; conflicts between those are caught by the safeAddRoute safety
// net during tree construction.
func segKind(seg string) (int, string) {
	if seg == "" {
		return segStatic, ""
	}
	switch seg[0] {
	case ':':
		return segParam, seg
	case '*':
		return segCatchAll, seg
	}
	return segStatic, ""
}

// segNode is a node of the analytical segment trie used to detect conflicts
// between rules without building (or mutating) a real route tree.
type segNode struct {
	static       map[string]*segNode
	param        *segNode
	paramName    string
	paramRule    int
	catchAll     bool
	catchAllName string
	catchAllRule int
	rule         int // index of the rule terminating here, -1 if none
}

func newSegNode() *segNode {
	return &segNode{rule: -1}
}

// collectRules gathers up to cap rule indices terminating at or below this
// node, in a deterministic order.
func (n *segNode) collectRules(out []int, cap int) []int {
	if len(out) >= cap {
		return out
	}
	if n.rule >= 0 {
		out = append(out, n.rule)
	}
	if n.catchAll && len(out) < cap {
		out = append(out, n.catchAllRule)
	}
	if n.static != nil {
		keys := make([]string, 0, len(n.static))
		for k := range n.static {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = n.static[k].collectRules(out, cap)
		}
	}
	if n.param != nil {
		out = n.param.collectRules(out, cap)
	}
	return out
}

// maxDiagRules bounds the number of involved rules listed in one diagnostic.
const maxDiagRules = 8

// analyzeRules validates all rules and detects duplicates, parameter name
// conflicts, unreachable branches and catch-all shadows in a single pass
// over a detached segment trie. Every independent conflict is reported: a
// conflicting rule is diagnosed against the already-accepted rules and then
// rejected, so it cannot cause cascading follow-up conflicts.
func analyzeRules(rules []Rule) Diagnostics {
	var diags Diagnostics

	valid := make([]bool, len(rules))
	for i := range rules {
		rule := &rules[i]
		if msg := validateRule(rule); msg != "" {
			diags = append(diags, Diagnostic{
				Kind:    DiagInvalid,
				Method:  rule.Method,
				Rules:   []string{ruleName(rule, i)},
				Paths:   []string{rule.Path},
				Sample:  samplePath(rule.Path),
				Message: fmt.Sprintf("httprouter: invalid rule %q (%s %s): %s", ruleName(rule, i), rule.Method, rule.Path, msg),
			})
		} else {
			valid[i] = true
		}
	}

	roots := make(map[string]*segNode)
	for i := range rules {
		if !valid[i] {
			continue
		}
		root := roots[rules[i].Method]
		if root == nil {
			root = newSegNode()
			roots[rules[i].Method] = root
		}
		if d := findConflict(root, rules, i); d != nil {
			diags = append(diags, *d)
			continue
		}
		applySegments(root, rules[i], i)
	}

	return sortDiagnostics(diags)
}

// ruleRefs formats the given rules as `"name" (METHOD path)` references.
func ruleRefs(rules []Rule, idxs ...int) []string {
	refs := make([]string, 0, len(idxs))
	for _, i := range idxs {
		refs = append(refs, fmt.Sprintf("%q (%s %s)", ruleName(&rules[i], i), rules[i].Method, rules[i].Path))
	}
	return refs
}

func makeDiag(kind DiagKind, method, sample, msg string, rules []Rule, idxs ...int) *Diagnostic {
	d := &Diagnostic{Kind: kind, Method: method, Sample: sample, Message: "httprouter: " + msg}
	for _, i := range idxs {
		d.Rules = append(d.Rules, ruleName(&rules[i], i))
		d.Paths = append(d.Paths, rules[i].Path)
	}
	return d
}

// findConflict walks the analytical segment trie read-only and returns a
// diagnostic describing the first conflict rule i would cause, or nil if the
// rule can be registered cleanly.
func findConflict(root *segNode, rules []Rule, i int) *Diagnostic {
	rule := &rules[i]
	sample := samplePath(rule.Path)
	req := rule.Method + " " + sample

	segs := strings.Split(rule.Path[1:], "/")
	cur := root
	for _, seg := range segs {
		// A catch-all on the path to this segment shadows everything below.
		if cur.catchAll {
			refs := ruleRefs(rules, i, cur.catchAllRule)
			return makeDiag(DiagCatchAllShadow, rule.Method, sample,
				fmt.Sprintf("catch-all shadow: rule %s is shadowed by catch-all rule %s (sample request: %s)", refs[0], refs[1], req),
				rules, i, cur.catchAllRule)
		}

		switch kind, wildcard := segKind(seg); kind {
		case segParam:
			if len(cur.static) > 0 {
				others := cur.collectRules(nil, maxDiagRules-1)
				refs := ruleRefs(rules, i, others[0])
				idxs := append([]int{i}, others...)
				return makeDiag(DiagUnreachableBranch, rule.Method, sample,
					fmt.Sprintf("unreachable branch: parameter segment of rule %s conflicts with static branch of rule %s (sample request: %s)", refs[0], refs[1], req),
					rules, idxs...)
			}
			if cur.param != nil && cur.paramName != wildcard {
				refs := ruleRefs(rules, cur.paramRule, i)
				return makeDiag(DiagParamNameConflict, rule.Method, sample,
					fmt.Sprintf("parameter name conflict: rule %s and rule %s use different parameter names for the same segment (sample request: %s)", refs[0], refs[1], req),
					rules, cur.paramRule, i)
			}
			if cur.param == nil {
				return nil // no existing branch below, no conflict
			}
			cur = cur.param

		case segCatchAll:
			if cur.catchAll {
				if cur.catchAllName != wildcard {
					refs := ruleRefs(rules, cur.catchAllRule, i)
					return makeDiag(DiagParamNameConflict, rule.Method, sample,
						fmt.Sprintf("parameter name conflict: rule %s and rule %s use different catch-all names for the same path (sample request: %s)", refs[0], refs[1], req),
						rules, cur.catchAllRule, i)
				}
				refs := ruleRefs(rules, cur.catchAllRule, i)
				return makeDiag(DiagDuplicate, rule.Method, sample,
					fmt.Sprintf("duplicate route: rule %s and rule %s register the same path (sample request: %s)", refs[0], refs[1], req),
					rules, cur.catchAllRule, i)
			}
			if len(cur.static) > 0 || cur.param != nil {
				others := cur.collectRules(nil, maxDiagRules-1)
				idxs := append([]int{i}, others...)
				refs := ruleRefs(rules, idxs...)
				shadowed := samplePath(rules[others[0]].Path)
				return makeDiag(DiagCatchAllShadow, rule.Method, shadowed,
					fmt.Sprintf("catch-all shadow: catch-all rule %s shadows rule %s (sample request: %s %s)", refs[0], refs[1], rule.Method, shadowed),
					rules, idxs...)
			}
			return nil

		default: // static
			if cur.param != nil {
				refs := ruleRefs(rules, i, cur.paramRule)
				return makeDiag(DiagUnreachableBranch, rule.Method, sample,
					fmt.Sprintf("unreachable branch: static segment of rule %s can never be reached because rule %s claims the same segment with a parameter (sample request: %s)", refs[0], refs[1], req),
					rules, i, cur.paramRule)
			}
			child := cur.static[seg]
			if child == nil {
				return nil // no existing branch below, no conflict
			}
			cur = child
		}
	}

	if cur.rule >= 0 {
		refs := ruleRefs(rules, cur.rule, i)
		return makeDiag(DiagDuplicate, rule.Method, sample,
			fmt.Sprintf("duplicate route: rule %s and rule %s register the same path (sample request: %s)", refs[0], refs[1], req),
			rules, cur.rule, i)
	}
	return nil
}

// applySegments inserts rule i into the analytical segment trie. It must
// only be called after findConflict reported no conflict for the rule.
func applySegments(root *segNode, rule Rule, i int) {
	cur := root
	for _, seg := range strings.Split(rule.Path[1:], "/") {
		switch kind, wildcard := segKind(seg); kind {
		case segParam:
			if cur.param == nil {
				cur.param = newSegNode()
				cur.paramName = wildcard
				cur.paramRule = i
			}
			cur = cur.param
		case segCatchAll:
			cur.catchAll = true
			cur.catchAllName = wildcard
			cur.catchAllRule = i
			return // catch-all is terminal (validated)
		default: // static
			child := cur.static[seg]
			if child == nil {
				if cur.static == nil {
					cur.static = make(map[string]*segNode)
				}
				child = newSegNode()
				cur.static[seg] = child
			}
			cur = child
		}
	}
	cur.rule = i
}

// sortDiagnostics sorts diagnostics deterministically, so that compiling the
// same rule set always reports conflicts in the same order.
func sortDiagnostics(ds Diagnostics) Diagnostics {
	less := func(x, y Diagnostic) bool {
		if x.Method != y.Method {
			return x.Method < y.Method
		}
		if x.Sample != y.Sample {
			return x.Sample < y.Sample
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return strings.Join(x.Rules, "\x00") < strings.Join(y.Rules, "\x00")
	}
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && less(ds[j], ds[j-1]); j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds
}
