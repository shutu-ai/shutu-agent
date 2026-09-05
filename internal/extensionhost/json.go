package extensionhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func marshalJSONLine(value any) ([]byte, error) {
	return marshalJSON(value)
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func timeout(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}
