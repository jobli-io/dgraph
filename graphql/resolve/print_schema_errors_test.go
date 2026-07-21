package resolve

import (
	"fmt"
	"os"
	"testing"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestPrintSchemaErrors(t *testing.T) {
	// Mock lambda URL so the parser accepts @lambda directives
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")

	schemaBytes, err := os.ReadFile("/Users/idowuayoola/Documents/jobli/graph/.graphql")
	if err != nil {
		t.Fatal(err)
	}

	_, parseErr := schema.NewHandler(string(schemaBytes), false)
	if parseErr != nil {
		fmt.Printf("Schema NewHandler failed with error:\n%s\n", parseErr.Error())
		t.Fatal("schema compile failed")
	} else {
		fmt.Println("Schema compiled successfully!")
	}
}
