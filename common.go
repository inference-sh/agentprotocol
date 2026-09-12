package agentprotocol

import "encoding/json"

type StringEncodedMap map[string]any

func (m *StringEncodedMap) UnmarshalJSON(data []byte) error {
	// First try to unmarshal directly as a map
	var rawMap map[string]any
	err := json.Unmarshal(data, &rawMap)
	if err == nil {
		*m = StringEncodedMap(rawMap)
		return nil
	}

	// If that fails, try to unmarshal as a string containing JSON
	var jsonStr string
	if err := json.Unmarshal(data, &jsonStr); err != nil {
		return err
	}

	// Then parse that string as JSON
	if err := json.Unmarshal([]byte(jsonStr), &rawMap); err != nil {
		return err
	}

	*m = StringEncodedMap(rawMap)
	return nil
}

// GetString retrieves a string value from the map
func (m StringEncodedMap) GetString(key string) (string, bool) {
	val, ok := m[key].(string)
	return val, ok
}

// GetBool retrieves a bool value from the map
func (m StringEncodedMap) GetBool(key string) (bool, bool) {
	val, ok := m[key].(bool)
	return val, ok
}

// GetInt retrieves an int value from the map (handles JSON numbers as float64)
func (m StringEncodedMap) GetInt(key string) (int, bool) {
	switch v := m[key].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case int64:
		return int(v), true
	}
	return 0, false
}

// GetFloat64 retrieves a float64 value from the map
func (m StringEncodedMap) GetFloat64(key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// GetSlice retrieves a slice value from the map
func (m StringEncodedMap) GetSlice(key string) ([]any, bool) {
	val, ok := m[key].([]any)
	return val, ok
}

// GetMap retrieves a nested map as StringEncodedMap
func (m StringEncodedMap) GetMap(key string) (StringEncodedMap, bool) {
	val, ok := m[key].(map[string]any)
	if !ok {
		return nil, false
	}
	return StringEncodedMap(val), true
}

// AsStringEncodedMap converts any value to StringEncodedMap if possible
func AsStringEncodedMap(v any) (StringEncodedMap, bool) {
	val, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return StringEncodedMap(val), true
}

func (m *StringEncodedMap) ToJSON() (json.RawMessage, bool) {
	jsonString, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(jsonString), true
}

func (m *StringEncodedMap) ToJSONBytes() ([]byte, error) {
	return json.Marshal(m)
}

func (m *StringEncodedMap) ToString() (string, error) {
	jsonBytes, err := m.ToJSONBytes()
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}
