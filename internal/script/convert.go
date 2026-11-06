package script

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.starlark.net/starlark"
)

// fromJSON converts a JSON document to Starlark values, keeping the key order
// of objects (the order the API returned them in).
func fromJSON(data []byte) (starlark.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (starlark.Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			d := starlark.NewDict(8)
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if err := d.SetKey(starlark.String(k), v); err != nil {
					return nil, err
				}
			}
			_, err := dec.Token() // '}'
			return d, err
		case '[':
			var elems []starlark.Value
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				elems = append(elems, v)
			}
			_, err := dec.Token() // ']'
			return starlark.NewList(elems), err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return starlark.String(t), nil
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return starlark.MakeInt64(i), nil
			}
			if b, ok := new(big.Int).SetString(s, 10); ok {
				return starlark.MakeBigInt(b), nil
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, err
		}
		return starlark.Float(f), nil
	case bool:
		return starlark.Bool(t), nil
	case nil:
		return starlark.None, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

// toJSON converts a Starlark value (dict, list, tuple, str, int, float, bool,
// None) to JSON, keeping dict insertion order.
func toJSON(v starlark.Value) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, v, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeValue(buf *bytes.Buffer, v starlark.Value, depth int) error {
	if depth > 100 {
		return fmt.Errorf("value nested too deeply")
	}
	switch x := v.(type) {
	case starlark.NoneType:
		buf.WriteString("null")
	case starlark.Bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case starlark.Int:
		buf.WriteString(x.String())
	case starlark.Float:
		b, err := json.Marshal(float64(x))
		if err != nil {
			return fmt.Errorf("cannot encode float %v as JSON", x)
		}
		buf.Write(b)
	case starlark.String:
		b, _ := json.Marshal(string(x))
		buf.Write(b)
	case starlark.Bytes:
		if !utf8.ValidString(string(x)) {
			return fmt.Errorf("bytes value is not valid UTF-8; pass base64 text instead")
		}
		b, _ := json.Marshal(string(x))
		buf.Write(b)
	case *starlark.Dict:
		buf.WriteByte('{')
		for i, item := range x.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return fmt.Errorf("dict key %s is not a string", item[0].String())
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			b, _ := json.Marshal(string(k))
			buf.Write(b)
			buf.WriteByte(':')
			if err := encodeValue(buf, item[1], depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case starlark.Indexable: // list, tuple
		if _, isStr := v.(starlark.String); isStr {
			break
		}
		buf.WriteByte('[')
		for i := 0; i < x.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, x.Index(i), depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("cannot convert %s to JSON", v.Type())
	}
	return nil
}
