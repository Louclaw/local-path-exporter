package scan

import "testing"

func TestGateAllowsOneFullWalk(t *testing.T) {
	var g Gate
	g.Arm("/a")
	if err := g.Allow("/a", true); err != nil {
		t.Fatal(err)
	}
	if err := g.Allow("/b", true); err != ErrDeferred {
		t.Fatalf("err = %v", err)
	}
	if err := g.Allow("/b", false); err != nil {
		t.Fatal(err)
	}
	g.Arm("")
	if err := g.Allow("/a", true); err != ErrDeferred {
		t.Fatalf("empty arm should defer, err = %v", err)
	}
}
