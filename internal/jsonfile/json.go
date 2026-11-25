// Package jsonfile owns strict, bounded operator JSON decoding. It does not
// select paths, authorize access, interpret credentials, or validate semantics.
package jsonfile

import (
	"encoding/json"
	"errors"
	"io"
	"os"
)

// Decode reads exactly one strict JSON value. Its errors keep a generic message
// but wrap their cause, so a caller can tell a reader failure, such as an
// exceeded request body limit, from malformed input.
func Decode(reader io.Reader, out any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return &decodeError{"invalid JSON", err}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return &decodeError{"one JSON value required", err}
	}
	return nil
}

type decodeError struct {
	message string
	cause   error
}

func (e *decodeError) Error() string { return e.message }
func (e *decodeError) Unwrap() error { return e.cause }

func Load(path string, maximum int64, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return errors.New("configuration exceeds its regular-file byte budget")
	}
	reader := &io.LimitedReader{R: file, N: maximum + 1}
	if err = Decode(reader, out); err != nil {
		return err
	}
	if reader.N <= 0 {
		return errors.New("configuration exceeds its byte budget")
	}
	return nil
}
