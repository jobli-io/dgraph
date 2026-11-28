// Command schemacheck loads a GraphQL schema file the way an alpha does when the
// schema is deployed, and prints what the load refuses, one line each. It starts
// nothing and writes nothing.
//
//	go run ./contrib/partition-spike/schemacheck ../graph/.graphql
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dgraph-io/gqlparser/v2/gqlerror"

	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
	"github.com/dgraph-io/ristretto/v2/z"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schemacheck <schema file>")
		os.Exit(2)
	}
	text, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// the load only asks that a lambda URL is set; nothing is called
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:1/graphql-worker;").MergeAndCheckDefault("lambda-url=;")
	h, err := schema.NewHandler(string(text), false)
	if err == nil {
		_, err = schema.FromString(h.GQLSchema(), 0)
	}
	if err == nil {
		fmt.Println("the schema loads")
		return
	}
	var lines []string
	if list, ok := err.(gqlerror.List); ok {
		for _, e := range list {
			lines = append(lines, strings.Join(strings.Fields(e.Error()), " "))
		}
	} else {
		lines = append(lines, strings.Join(strings.Fields(err.Error()), " "))
	}
	sort.Strings(lines)
	// An expression that calls lambda is tried out at load, and there is no lambda
	// here to call: those are counted apart and are not a verdict on the schema.
	refused, lambda := 0, 0
	for _, l := range lines {
		if strings.Contains(l, "callLambda") && strings.Contains(l, "expression execution failed") {
			lambda++
			continue
		}
		refused++
		fmt.Println(l)
	}
	fmt.Printf("%d refused; %d expressions that call lambda were not tried\n", refused, lambda)
	if refused > 0 {
		os.Exit(1)
	}
}
