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

func TestDeriveSlots(t *testing.T) {
	got := deriveSlots("Hi {name}, welcome to {org}. Bye {name}.")
	if want := []string{"name", "org"}; !reflect.DeepEqual(got, want) { // deduped, in order
		t.Errorf("deriveSlots = %v, want %v", got, want)
	}
	if s := deriveSlots("no slots here"); len(s) != 0 {
		t.Errorf("deriveSlots on plain text = %v, want empty", s)
	}
}
