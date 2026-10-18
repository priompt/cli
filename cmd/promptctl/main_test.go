package main

import (
	"reflect"
	"testing"
)

func TestPathToURI(t *testing.T) {
	got := pathToURI("acme/onboarding/welcome.prompt")
	if want := "priompt://acme/onboarding/welcome"; got != want {
		t.Errorf("pathToURI = %q, want %q", got, want)
	}
}

func TestChangedLines(t *testing.T) {
	cases := []struct {
		name, old, neu string
		want           bool
	}{
		{"identical", "a\nb\n", "a\nb\n", false},
		{"crlf vs lf", "a\r\nb\r\n", "a\nb\n", false},
		{"lf vs crlf", "a\nb\n", "a\r\nb\r\n", false},
		{"mixed endings", "a\r\nb\n", "a\nb\r\n", false},
		{"text change", "a\r\nb\r\n", "a\nc\n", true},
		{"trailing newline added", "a\nb", "a\r\nb\r\n", true},
	}
	for _, c := range cases {
		a, b, changed := changedLines(c.old, c.neu)
		if changed != c.want {
			t.Errorf("%s: changed = %v, want %v", c.name, changed, c.want)
		}
		if !reflect.DeepEqual(a, splitLines(c.old)) || !reflect.DeepEqual(b, splitLines(c.neu)) {
			t.Errorf("%s: lines = %q, %q, want normalised split of inputs", c.name, a, b)
		}
	}
}

func TestDeriveSlots(t *testing.T) {
	got := deriveSlots("Hi {name}, welcome to {org}. Bye {name}.")
	if want := []string{"name", "org"}; !reflect.DeepEqual(got, want) { // deduped, in order
		t.Errorf("deriveSlots = %v, want %v", got, want)
	}
	if s := deriveSlots("no slots here"); len(s) != 0 {
		t.Errorf("deriveSlots on plain text = %v, want empty", s)
	}
}
