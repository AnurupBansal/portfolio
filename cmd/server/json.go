package main

import (
	"encoding/json"
	"io"
)

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ") // Readable when someone curls it directly.
	return enc.Encode(v)
}
