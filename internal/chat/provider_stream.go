package chat

import (
	"bufio"
	"io"
	"strings"
)

// Counts generated material without making private reasoning an answer or event.
// Count decoded material, never repeated SSE metadata or keepalives.
type providerObservation struct{ outputBytes, reasoningBytes, limit int }

func (o *providerObservation) add(text string, reasoning bool) error {
	n := len(text)
	if n > o.limit-o.outputBytes {
		o.outputBytes = o.limit
		return errProviderLimit
	}
	o.outputBytes += n
	if reasoning {
		o.reasoningBytes += n
	}
	return nil
}
func (o providerObservation) apply(r *Response) {
	if o.outputBytes > r.observedOutputBytes {
		r.observedOutputBytes = o.outputBytes
	}
	if o.reasoningBytes > r.observedReasoningBytes {
		r.observedReasoningBytes = o.reasoningBytes
	}
}

// Bound individual frames, not the total stream. Long reasoning responses can
// carry far more framing bytes than generated text. The request context owns
// their lifetime; retained content has its own limits.
func scanProviderSSE(r io.Reader, limit int, handle func([]byte) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, min(4096, limit)), limit)
	data := []byte{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				done, err := handle(data)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
				data = data[:0]
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
			if len(data) > limit {
				return errProviderLimit
			}
		}
	}
	if scanner.Err() != nil {
		return errProviderData
	}
	return errProviderData
}
func (p *Provider) readStream(r io.Reader, emit func(Event) error) (Response, error) {
	return p.wire.stream(p, r, emit)
}
