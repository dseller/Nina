package oas

import (
	"os"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	yaml "go.yaml.in/yaml/v4"
)

// Contract 1: can libopenapi give us typed access to operations for route-table building?
func TestContractTypedAccess(t *testing.T) {
	b, err := os.ReadFile("testdata/users.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := libopenapi.NewDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	model, errs := doc.BuildV3Model()
	if errs != nil {
		t.Fatalf("build model: %v", errs)
	}
	t.Logf("version=%s title=%s", model.Model.Version, model.Model.Info.Title)
	for p := model.Model.Paths.PathItems.First(); p != nil; p = p.Next() {
		for o := p.Value().GetOperations().First(); o != nil; o = o.Next() {
			t.Logf("  %s %s opID=%s params=%d",
				strings.ToUpper(o.Key()), p.Key(), o.Value().OperationId, len(o.Value().Parameters))
		}
	}
}

// Contract 2: does the raw yaml.Node tree survive a round-trip with $refs intact?
// This is the fallback merge strategy from the plan; if it holds, it is also the
// primary one, because it is lossless and we control every rewrite.
func TestContractRawTreeRoundTrip(t *testing.T) {
	b, err := os.ReadFile("testdata/users.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := libopenapi.NewDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	root := doc.GetSpecInfo().RootNode
	if root == nil {
		t.Fatal("no root node from libopenapi SpecInfo")
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"$ref: '#/components/schemas/User'",
		"$ref: '#/components/parameters/TraceId'",
		"operationId: listUsers",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("round-trip lost %q", want)
		}
	}
	t.Logf("round-trip output %d bytes (input %d)", len(out), len(b))
}

// Contract 3: can we rewrite a $ref string in the tree and re-render?
func TestContractRefRewrite(t *testing.T) {
	b, _ := os.ReadFile("testdata/users.yaml")
	doc, err := libopenapi.NewDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	root := doc.GetSpecInfo().RootNode

	var n int
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				k, v := node.Content[i], node.Content[i+1]
				if k.Value == "$ref" && v.Kind == yaml.ScalarNode {
					v.Value = strings.Replace(v.Value, "#/components/schemas/", "#/components/schemas/Users", 1)
					n++
				}
				walk(v)
			}
			return
		}
		for _, c := range node.Content {
			walk(c)
		}
	}
	walk(root)

	out, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "#/components/schemas/UsersUser") {
		t.Fatalf("rewrite did not land; got:\n%s", out)
	}
	t.Logf("rewrote %d refs", n)
}
