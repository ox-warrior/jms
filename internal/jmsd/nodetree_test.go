package jmsd

import (
	"reflect"
	"testing"

	"github.com/jumpserver-dev/sdk-go/model"
)

func TestBuildNodeTree(t *testing.T) {
	t.Parallel()
	nodes := []model.Node{
		{ID: "test-id", Key: "1:3:1", Name: "Beijing"},
		{ID: "prod-id", Key: "1:3", Name: "Production"},
		{ID: "sh-id", Key: "1:3:0", Name: "Shanghai"},
		{ID: "staging-id", Key: "1:4", Name: "Staging"},
	}

	got := BuildNodeTree(nodes)

	want := []NodeTree{
		{ID: "prod-id", Name: "Production", Children: []NodeTree{
			{ID: "sh-id", Name: "Shanghai"},
			{ID: "test-id", Name: "Beijing"},
		}},
		{ID: "staging-id", Name: "Staging"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildNodeTree() = %#v, want %#v", got, want)
	}
}

func TestBuildNodeTreeOrphanBecomesRoot(t *testing.T) {
	t.Parallel()
	// The user's grant starts partway down the tree: "1:3" (its parent, "1")
	// is not in the input, so it must surface as a top-level entry rather
	// than being dropped.
	nodes := []model.Node{
		{ID: "child-id", Key: "1:3:0", Name: "Shanghai"},
		{ID: "mid-id", Key: "1:3", Name: "Production"},
	}

	got := BuildNodeTree(nodes)

	want := []NodeTree{
		{ID: "mid-id", Name: "Production", Children: []NodeTree{
			{ID: "child-id", Name: "Shanghai"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildNodeTree() = %#v, want %#v", got, want)
	}
}

func TestBuildNodeTreeEmpty(t *testing.T) {
	t.Parallel()
	got := BuildNodeTree(nil)
	if len(got) != 0 {
		t.Fatalf("BuildNodeTree(nil) = %#v, want empty", got)
	}
}
