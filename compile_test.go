// Copyright 2013 Julien Schmidt. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// in the LICENSE file.

package httprouter

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func compileHandler(tag string) Handle {
	return func(w http.ResponseWriter, r *http.Request, _ Params) {
		w.Write([]byte(tag))
	}
}

func TestCompileAndPublish(t *testing.T) {
	r := New()
	r.GET("/old", compileHandler("old"))

	snap, diags := r.CompileRules(
		Rule{Name: "index", Method: "GET", Path: "/", Handle: compileHandler("index")},
		Rule{Name: "user", Method: "GET", Path: "/user/:id", Handle: compileHandler("user")},
		Rule{Name: "files", Method: "GET", Path: "/files/*filepath", Handle: compileHandler("files")},
	)
	if diags != nil {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if snap == nil {
		t.Fatal("expected snapshot")
	}

	// Before Publish the old table is still active.
	if h, _, _ := r.Lookup("GET", "/old"); h == nil {
		t.Fatal("old route missing before publish")
	}
	if h, _, _ := r.Lookup("GET", "/user/1"); h != nil {
		t.Fatal("new route visible before publish")
	}

	if changed := r.Publish(snap); !changed {
		t.Fatal("expected publish to report a change")
	}

	// Idempotent: publishing the same content again changes nothing.
	if changed := r.Publish(snap); changed {
		t.Fatal("expected re-publish of same snapshot to be a no-op")
	}

	// Old route is gone, new routes work with params.
	if h, _, _ := r.Lookup("GET", "/old"); h != nil {
		t.Fatal("old route still active after publish")
	}
	h, ps, _ := r.Lookup("GET", "/user/42")
	if h == nil {
		t.Fatal("new route missing after publish")
	}
	if got := ps.ByName("id"); got != "42" {
		t.Fatalf("param id = %q, want %q", got, "42")
	}

	// Catch-all and TSR semantics are preserved.
	if h, ps, _ := r.Lookup("GET", "/files/a/b.txt"); h == nil || ps.ByName("filepath") != "/a/b.txt" {
		t.Fatal("catch-all lookup broken after publish")
	}
	if _, _, tsr := r.Lookup("GET", "/user/42/"); !tsr {
		t.Fatal("expected TSR recommendation after publish")
	}

	// End-to-end via ServeHTTP, including redirect semantics.
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/user/7", nil)
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "user" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "user")
	}

	rec = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/user/7/", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("redirect status = %d, want %d", rec.Code, http.StatusMovedPermanently)
	}
}

func TestCompileDuplicate(t *testing.T) {
	r := New()
	_, diags := r.CompileRules(
		Rule{Name: "a", Method: "GET", Path: "/user/:id", Handle: compileHandler("a")},
		Rule{Name: "b", Method: "GET", Path: "/user/:id", Handle: compileHandler("b")},
	)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %v", len(diags), diags)
	}
	d := diags[0]
	if d.Kind != DiagDuplicate {
		t.Fatalf("kind = %v, want DiagDuplicate", d.Kind)
	}
	if !reflect.DeepEqual(d.Rules, []string{"a", "b"}) {
		t.Fatalf("rules = %v", d.Rules)
	}
	if d.Sample != "/user/{id}" {
		t.Fatalf("sample = %q", d.Sample)
	}
}

func TestCompileParamNameConflict(t *testing.T) {
	r := New()
	_, diags := r.CompileRules(
		Rule{Name: "by-id", Method: "GET", Path: "/user/:id", Handle: compileHandler("a")},
		Rule{Name: "by-name", Method: "GET", Path: "/user/:name", Handle: compileHandler("b")},
	)
	if len(diags) != 1 || diags[0].Kind != DiagParamNameConflict {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if !reflect.DeepEqual(diags[0].Rules, []string{"by-id", "by-name"}) {
		t.Fatalf("rules = %v", diags[0].Rules)
	}
}

func TestCompileUnreachableBranch(t *testing.T) {
	r := New()
	for _, rules := range [][]Rule{
		{
			{Name: "show", Method: "GET", Path: "/user/:id", Handle: compileHandler("a")},
			{Name: "new", Method: "GET", Path: "/user/new", Handle: compileHandler("b")},
		},
		{
			// registration order must not matter
			{Name: "new", Method: "GET", Path: "/user/new", Handle: compileHandler("b")},
			{Name: "show", Method: "GET", Path: "/user/:id", Handle: compileHandler("a")},
		},
	} {
		_, diags := r.CompileRules(rules...)
		if len(diags) != 1 || diags[0].Kind != DiagUnreachableBranch {
			t.Fatalf("unexpected diagnostics for %v: %v", rules, diags)
		}
		if !reflect.DeepEqual(diags[0].Rules, []string{"new", "show"}) &&
			!reflect.DeepEqual(diags[0].Rules, []string{"show", "new"}) {
			t.Fatalf("rules = %v", diags[0].Rules)
		}
	}
}

func TestCompileCatchAllShadow(t *testing.T) {
	r := New()
	_, diags := r.CompileRules(
		Rule{Name: "assets", Method: "GET", Path: "/files/*filepath", Handle: compileHandler("a")},
		Rule{Name: "readme", Method: "GET", Path: "/files/readme", Handle: compileHandler("b")},
		Rule{Name: "dir", Method: "GET", Path: "/files/", Handle: compileHandler("c")},
	)
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %v", len(diags), diags)
	}
	for _, d := range diags {
		if d.Kind != DiagCatchAllShadow {
			t.Fatalf("kind = %v, want DiagCatchAllShadow", d.Kind)
		}
	}
	if diags[0].Sample != "/files/" || diags[1].Sample != "/files/readme" {
		t.Fatalf("samples = %q, %q", diags[0].Sample, diags[1].Sample)
	}

	// Reverse registration order: the catch-all is the shadowing rule.
	_, diags = r.CompileRules(
		Rule{Name: "readme", Method: "GET", Path: "/files/readme", Handle: compileHandler("b")},
		Rule{Name: "assets", Method: "GET", Path: "/files/*filepath", Handle: compileHandler("a")},
	)
	if len(diags) != 1 || diags[0].Kind != DiagCatchAllShadow {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if !reflect.DeepEqual(diags[0].Rules, []string{"assets", "readme"}) {
		t.Fatalf("rules = %v", diags[0].Rules)
	}
}

func TestCompileMultipleIndependentConflicts(t *testing.T) {
	rules := []Rule{
		{Name: "dup-a", Method: "GET", Path: "/a", Handle: compileHandler("a")},
		{Name: "dup-b", Method: "GET", Path: "/a", Handle: compileHandler("b")},
		{Name: "param-a", Method: "POST", Path: "/p/:x", Handle: compileHandler("c")},
		{Name: "param-b", Method: "POST", Path: "/p/:y", Handle: compileHandler("d")},
		{Name: "invalid", Method: "", Path: "/z", Handle: compileHandler("e")},
	}
	r := New()
	_, diags1 := r.CompileRules(rules...)
	if len(diags1) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d: %v", len(diags1), diags1)
	}
	// Error order is stable across compilations of the same rule set.
	_, diags2 := r.CompileRules(rules...)
	if !reflect.DeepEqual(diags1, diags2) {
		t.Fatalf("unstable diagnostic order:\n%v\n%v", diags1, diags2)
	}
	// Diagnostics implement error and mention the involved rules.
	msg := diags1.Error()
	for _, want := range []string{"dup-a", "dup-b", "param-a", "param-b", "invalid"} {
		if !contains(msg, want) {
			t.Fatalf("error message missing %q:\n%s", want, msg)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCompileFailureKeepsOldSnapshot(t *testing.T) {
	r := New()
	r.GET("/old", compileHandler("old"))

	snap, diags := r.CompileRules(
		Rule{Name: "a", Method: "GET", Path: "/x", Handle: compileHandler("a")},
		Rule{Name: "b", Method: "GET", Path: "/x", Handle: compileHandler("b")},
	)
	if diags == nil || snap != nil {
		t.Fatalf("expected failed compile, got snap=%v diags=%v", snap, diags)
	}

	// The active table is untouched.
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/old", nil)
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "old" {
		t.Fatalf("old snapshot polluted: body = %q", rec.Body.String())
	}
	if h, _, _ := r.Lookup("GET", "/x"); h != nil {
		t.Fatal("failed compile leaked route into active table")
	}
}

func TestCompileInvalidRules(t *testing.T) {
	r := New()
	_, diags := r.CompileRules(
		Rule{Name: "no-method", Method: "", Path: "/a", Handle: compileHandler("a")},
		Rule{Name: "no-slash", Method: "GET", Path: "a", Handle: compileHandler("a")},
		Rule{Name: "no-handle", Method: "GET", Path: "/b", Handle: nil},
		Rule{Name: "mid-catchall", Method: "GET", Path: "/c/*rest/more", Handle: compileHandler("a")},
		Rule{Name: "unnamed", Method: "GET", Path: "/d/:", Handle: compileHandler("a")},
	)
	if len(diags) != 5 {
		t.Fatalf("expected 5 diagnostics, got %d: %v", len(diags), diags)
	}
	for _, d := range diags {
		if d.Kind != DiagInvalid {
			t.Fatalf("kind = %v, want DiagInvalid (%v)", d.Kind, d)
		}
	}
}

func TestCompilerStagingAndClose(t *testing.T) {
	r := New()
	c := r.Compiler()
	c.Add("index", "GET", "/", compileHandler("index"))
	c.AddRule(Rule{Name: "user", Method: "GET", Path: "/user/:id", Handle: compileHandler("user")})

	snap, diags := c.Compile()
	if diags != nil || snap == nil {
		t.Fatalf("compile failed: %v", diags)
	}
	r.Publish(snap)
	if h, _, _ := r.Lookup("GET", "/user/1"); h == nil {
		t.Fatal("route missing after publish")
	}

	// Abandoning a compiler leaves no trace.
	c2 := r.Compiler()
	c2.Add("broken", "GET", "/user/:name", compileHandler("x"))
	c2.Close()
	if _, diags := c2.Compile(); diags == nil {
		t.Fatal("expected error compiling with closed compiler")
	}
	if h, _, _ := r.Lookup("GET", "/user/1"); h == nil {
		t.Fatal("active table changed after Close")
	}
}

func TestPublishConcurrentReads(t *testing.T) {
	r := New()
	r.GET("/a/:id", compileHandler("a"))

	snap, diags := r.CompileRules(
		Rule{Name: "b", Method: "GET", Path: "/b/:id", Handle: compileHandler("b")},
	)
	if diags != nil {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Lock-free reads of whichever snapshot is active.
				h1, ps1, _ := r.Lookup("GET", "/a/1")
				h2, ps2, _ := r.Lookup("GET", "/b/1")
				if h1 == nil && h2 == nil {
					t.Error("no active snapshot visible to reader")
					return
				}
				if h1 != nil && ps1.ByName("id") != "1" {
					t.Error("corrupt params from old snapshot")
					return
				}
				if h2 != nil && ps2.ByName("id") != "1" {
					t.Error("corrupt params from new snapshot")
					return
				}
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		r.Publish(snap)
	}
	close(stop)
	wg.Wait()
}

func TestSnapshotLookup(t *testing.T) {
	r := New()
	snap, diags := r.CompileRules(
		Rule{Name: "user", Method: "GET", Path: "/user/:id", Handle: compileHandler("user")},
	)
	if diags != nil {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	h, ps, tsr := snap.Lookup("GET", "/user/9")
	if h == nil || tsr {
		t.Fatal("snapshot lookup failed")
	}
	if ps.ByName("id") != "9" {
		t.Fatalf("param id = %q", ps.ByName("id"))
	}
	if _, _, tsr := snap.Lookup("GET", "/user/9/"); !tsr {
		t.Fatal("expected TSR recommendation from snapshot")
	}
}

func TestCompileRespectsSaveMatchedRoutePath(t *testing.T) {
	r := New()
	r.SaveMatchedRoutePath = true
	snap, diags := r.CompileRules(
		Rule{Name: "user", Method: "GET", Path: "/user/:id", Handle: func(w http.ResponseWriter, req *http.Request, ps Params) {
			w.Write([]byte(ps.MatchedRoutePath()))
		}},
	)
	if diags != nil {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	r.Publish(snap)

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/user/1", nil)
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "/user/:id" {
		t.Fatalf("matched route path = %q", rec.Body.String())
	}
}
