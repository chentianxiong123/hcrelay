package ssdp

import (
	"encoding/xml"
	"io"
)

func xmlDecode(r io.Reader, v any) error {
	return xml.NewDecoder(r).Decode(v)
}
