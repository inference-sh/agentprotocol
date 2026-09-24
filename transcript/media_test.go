package transcript

import "testing"

func TestDataURL(t *testing.T) {
	for _, c := range []struct{ in, mt, data string }{
		{DataURL("image/png", []byte{1, 2, 3}), "image/png", "\x01\x02\x03"},
		{"data:image/png;name=x.png;base64,AQID", "image/png", "\x01\x02\x03"},
		{"data:text/plain,a%20b", "text/plain", "a b"},
	} {
		mt, data, ok := ParseDataURL(c.in)
		if !ok || mt != c.mt || string(data) != c.data {
			t.Errorf("ParseDataURL(%q) = %q, %q, %v", c.in, mt, data, ok)
		}
	}
	for _, bad := range []string{"https://x/y.png", "data:image/png;base64", "data:image/png;base64,!!"} {
		if _, _, ok := ParseDataURL(bad); ok {
			t.Errorf("ParseDataURL(%q) ok", bad)
		}
	}
}
