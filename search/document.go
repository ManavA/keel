package search

// Document is anything that can be stored in the index. A struct
// implementing ID (whatever its JSON tags name that field) or a
// [MapDocument] both work; this package never assumes more about a
// document's shape than "it JSON-marshals, and it has an id."
type Document interface {
	// ID returns the document's primary-key value. It must match the JSON
	// field name given as Config.PrimaryKey when the document is marshaled.
	ID() string
}

// MapDocument is a schema-less [Document] backed by a plain map. ID reads
// the "id" key as a string; documents that key their primary key under a
// different JSON field should implement Document directly instead.
type MapDocument map[string]any

// ID returns the "id" key as a string, or "" if it is absent or not a
// string.
func (d MapDocument) ID() string {
	v, _ := d["id"].(string)
	return v
}
