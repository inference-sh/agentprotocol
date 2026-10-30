package tomlstr

import "testing"

func TestParse(t *testing.T) {
	got := Parse(`
top = "a" # comment
[t."x.y"]
s = 'lit'
e = "q\"é"
n = 3
arr = [1, "two", [3]]
inline = { a = "1", b = { c = "2" } }
dotted.k = "d"
[[arr_of_tables]]
k = "hidden"
`)
	for k, want := range map[string]string{
		Key("top"): "a", Key("t", "x.y", "s"): "lit", Key("t", "x.y", "e"): "q\"é",
		Key("t", "x.y", "inline", "a"): "1", Key("t", "x.y", "inline", "b", "c"): "2", Key("t", "x.y", "dotted", "k"): "d",
	} {
		if got[k] != want {
			t.Errorf("%q = %q, want %q", k, got[k], want)
		}
	}
	if got[Key("k")] != "" || got[Key("t", "x.y", "n")] != "" {
		t.Errorf("kept values it should not: %q", got)
	}
}
