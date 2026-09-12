package main

import "testing"

func TestPortForwardListParsesDocumentedForms(t *testing.T) {
	var forwards portForwardList
	for _, value := range []string{"3000", "8080:3000"} {
		if err := forwards.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}
	if len(forwards) != 2 || forwards[0].Local != 3000 || forwards[0].Remote != 3000 ||
		forwards[1].Local != 8080 || forwards[1].Remote != 3000 {
		t.Fatalf("parsed forwards = %+v", forwards)
	}
}

func TestPortForwardListRejectsAmbiguousOrInvalidForms(t *testing.T) {
	for _, value := range []string{"", "0", "65536", ":3000", "3000:", "localhost:3000", "1:2:3"} {
		t.Run(value, func(t *testing.T) {
			var forwards portForwardList
			if err := forwards.Set(value); err == nil {
				t.Fatalf("Set(%q) succeeded", value)
			}
		})
	}
}

func TestCmdRunRejectsDetachedForward(t *testing.T) {
	if code := cmdRun([]string{"--detach", "--forward", "3000", "--", "/bin/true"}); code != 2 {
		t.Fatalf("detached forward exit = %d, want 2", code)
	}
}

func TestCmdRunRejectsDetachedForwardShortFlags(t *testing.T) {
	if code := cmdRun([]string{"-d", "-L", "3000", "--", "/bin/true"}); code != 2 {
		t.Fatalf("detached forward short flags exit = %d, want 2", code)
	}
}
