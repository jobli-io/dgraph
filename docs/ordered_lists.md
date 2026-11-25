# Ordered String Lists (`stringvector` & `@ordered`)

Historically in Dgraph, `[string]` predicates are stored as unordered sets of postings keyed by
`farm.Fingerprint64(value)`, which deduplicates values and orders them by hash fingerprint rather
than insertion order.

Following the single-posting design introduced for `float32vector` (HNSW embeddings), Dgraph now
supports **ordered string lists** natively in both DQL (`stringvector`) and GraphQL
(`@ordered(count: Boolean)`). Ordered string lists preserve exact element order, duplicate elements,
empty strings (`""`), and explicit empty arrays (`[]`).

---

## 1. DQL: `stringvector`

### Schema Declaration

Declare an ordered string list predicate using the `stringvector` type. All standard string
tokenizers (`exact`, `hash`, `term`, `fulltext`, `trigram`, `ngram`) and the `@count` directive are
supported:

```dql
steps: stringvector @index(exact, term, fulltext, trigram) @count .
```

**Disallowed combinations:**

- `[stringvector]` (list-of-vectors is rejected, matching `[float32vector]`)
- `@lang` (language tags are not supported on `stringvector`)
- `@upsert` / `@unique`
- Sorting (`orderasc` / `orderdesc`) on `stringvector` predicates

### Binary Storage Layout

Each `stringvector` value is stored as a single `pb.Posting` (`ValType = Posting_VSTRING`,
`Uid = math.MaxUint64`, `SchemaUpdate.List = false`) using an Arrow-style offset table for $O(1)$
length lookup and zero-delimiter UTF-8 slicing:

```text
[count: uint32 LE][endOffsets: uint32[count] LE][concatenated UTF-8 bytes]
```

- Empty list `[]` is encoded as 4 zero bytes (`count = 0`), distinguishing an explicit empty list
  (`"steps": []`) from an unset/deleted predicate.
- Strings containing arbitrary UTF-8, quotes, newlines, or embedded NUL bytes (`\x00`) are stored
  losslessly without escaping overhead.

### Mutations (JSON & RDF)

Mutations on `stringvector` predicates have **whole-list replacement** semantics:

#### JSON Mutations

```json
{
  "set": [
    {
      "uid": "_:w1",
      "steps": ["draft", "review", "draft", "publish"]
    }
  ]
}
```

- **Overwrite:** Setting `"steps": ["archived"]` replaces the entire ordered list.
- **Clear to empty list (`[]`):** Setting `"steps": []` stores an empty 0-element list
  (`"steps": []` is returned when queried, and `count(steps)` is `0`). Note: on unordered `[string]`
  predicates, `"tags": []` remains a no-op.
- **Delete (`S P *`):** Deleting `"steps": null` in `DeleteJson` (or `<0x1> <steps> * .` in RDF)
  removes the predicate posting completely.
- **Safety guarantee:** JSON strings on regular `string` predicates (e.g., `"raw": "[\"a\",\"b\"]"`)
  are **never** sniffed or coerced into `stringvector`; coalescing happens strictly on the Alpha
  group using authoritative tablet schema metadata.

#### RDF N-Quad Mutations & Exports

In RDF, `stringvector` uses the `<xs:[]string>` datatype tag with a JSON-encoded string array
literal:

```rdf
_:w1 <steps> "[\"draft\",\"review\",\"draft\"]"^^<xs:[]string> .
```

- **RDF Export** emits `"[\"draft\",\"review\",\"draft\"]"^^<xs:[]string>`.
- **JSON Export** emits raw JSON arrays (`"steps":["draft","review","draft"]`) that round-trip
  cleanly through Live Loader and Bulk Loader.

### Querying, Filtering & Counting

All string index functions and `@count` operations evaluate across the elements of `stringvector`
(including duplicates for `count`):

```dql
{
  # Root index lookup (matches any node where at least one element matches)
  byExact(func: eq(steps, "review")) {
    uid
    steps
    stepCount: count(steps)
  }

  # Term matching across elements in the same ordered list
  byTerms(func: allofterms(steps, "draft publish")) {
    uid
    steps
  }

  # @count index lookup (counts total elements, including duplicates)
  threeOrMore(func: ge(count(steps), 3)) @filter(regexp(steps, /^pub.*/)) {
    uid
    steps
  }
}
```

When updating a `stringvector` value on an indexed predicate, Dgraph diffs the old and new token
sets (`tok.BuildTokens` deduplicated across elements) so shared tokens remain indexed without false
deletions.

### Online Schema Migration

Dgraph supports in-place online schema migration without dropping data:

- **`[string]` -> `stringvector`:** Existing unordered set elements are packed into a `stringvector`
  posting (initial order follows existing posting iteration order; subsequent writes preserve exact
  input order), and token/`@count` indexes are rebuilt automatically.
- **`stringvector` -> `[string]`:** Unpacks each ordered list into fingerprinted `Posting_STRING`
  set entries (deduplicating repeated values) and rebuilds `@count` / token indexes.
- **`string` -> `stringvector`:** Wraps each existing scalar string `v` into a 1-element ordered
  list `[v]`.

---

## 2. GraphQL: `@ordered` Directive

### Signature

```graphql
directive @ordered(count: Boolean) on FIELD_DEFINITION
```

### Supported Field Types

`@ordered` can be applied to list fields of `String` or any GraphQL `enum` type (all nullability
combinations: `[String]`, `[String!]`, `[String]!`, `[String!]!`, `[MyEnum!]!`, etc.), including on
interfaces:

```graphql
enum Stage {
  DRAFT
  REVIEW
  DONE
}

type Workflow {
  id: ID!
  steps: [String!] @ordered(count: true) @search(by: [exact, term])
  stages: [Stage!]! @ordered(count: true)
}
```

Generated DQL schema:

```dql
Workflow.steps: stringvector @index(exact, term) @count .
Workflow.stages: stringvector @index(hash) @count .
```

### GraphQL Mutation Semantics

| Operation              | Input                        | Behavior                                                                                |
| ---------------------- | ---------------------------- | --------------------------------------------------------------------------------------- |
| `addXxx`               | `steps: ["b", "a", "b"]`     | Stores `["b", "a", "b"]` in exact order with duplicates                                 |
| `addXxx`               | `steps: []`                  | Stores explicit empty list `[]`                                                         |
| `updateXxx` (`set`)    | `set: { steps: ["x", "y"] }` | Replaces the entire ordered list with `["x", "y"]`                                      |
| `updateXxx` (`set`)    | `set: { steps: [] }`         | Replaces the ordered list with empty list `[]`                                          |
| `updateXxx` (`remove`) | `remove: { steps: null }`    | Deletes the predicate (`S P *`)                                                         |
| `updateXxx` (`remove`) | `remove: { steps: ["a"] }`   | **Rejected** with GraphQL error (`cannot remove specific elements from @ordered field`) |
| `addXxx` / `updateXxx` | `steps: ["a", null]`         | **Rejected** with GraphQL error (`cannot store null element in @ordered field`)         |

### Compatibility with Other GraphQL Directives

- **`@search`:** Fully supported (`exact`, `hash`, `term`, `fulltext`, `trigram`, `regexp`,
  `ngram`).
- **`@default` / `@transform` / `@validate` / `@postValidate` / `@oldValue`:** Fully compatible; in
  `expr-lang` expressions, `@ordered` fields appear as ordered slices (`input.steps`,
  `before.steps`, `after.steps`) preserving element order and duplicates.
