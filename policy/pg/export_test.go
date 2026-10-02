package pg

// Query is the select that List runs for f, and its arguments, so that a test
// can ask the server how it will run exactly that statement.
func (f Filter) Query() (string, []any) { return f.query() }
