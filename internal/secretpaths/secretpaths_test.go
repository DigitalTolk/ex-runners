package secretpaths

import (
	"reflect"
	"testing"
)

func TestUnder(t *testing.T) {
	got := Under("/h", "/x/private", "", "/h/.ssh")
	if len(got) != len(Home)+1 || got[0] != "/h/.aws" || !contains(got, "/h/Library/Keychains") || !contains(got, "/x/private") {
		t.Fatalf("Under = %v", got)
	}
	if got := Under("", "/b", "/a", "/a"); !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Fatalf("no home = %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
