package confuse

import "testing"

func TestDefaultTables(t *testing.T) {
	d := Default()
	if c, s, sup, gen := d.Size(); c != 46 || s != 2 || sup != 11 || gen < 40 {
		t.Fatalf("sizes: %d chars, %d sequences, %d support, %d generic", c, s, sup, gen)
	}
	for _, p := range [][2]string{{"oscаr", "oscar"}, {"0scar", "oscar"}, {"o5car", "oscar"}, {"modern", "modem"}, {"vvalter", "walter"},
		{"bo​ris", "boris"}, {"ΤΟΚΙΟ", "tokio"}, {"j0hn", "john"}} {
		if d.Skeleton(p[0]) != d.Skeleton(p[1]) {
			t.Errorf("%s vs %s: %s %s", p[0], p[1], d.Skeleton(p[0]), d.Skeleton(p[1]))
		}
	}
	if d.Skeleton("oscar") == d.Skeleton("oscara") {
		t.Error("oscara is not oscar")
	}
	if d.Imitates("oscar-support", []string{"oscar"}, nil) != "oscar" || d.Imitates("dana", []string{"dan"}, nil) != "" {
		t.Error("imitates")
	}
	if d.MixedScriptErr("oscаr", false) == nil || d.MixedScriptErr("оскар", false) != nil || d.MixedScriptErr("oscаr", true) != nil {
		t.Error("mixed scripts")
	}
	if !d.Generic("wallet") || d.Generic("oscar") || d.SupportWord("annasupport") != "support" {
		t.Error("words")
	}
}

// A module's tables add to the built-in ones and can't take anything away.
func TestWithOnlyAdds(t *testing.T) {
	d := Default()
	x, err := d.With([]byte("0430 ; 0062 ; MA # tries to remap Cyrillic а to b\n0581 ; 0067 ; MA # Armenian ց as g\n0063 006C ; 0064 ; MA # cl as d\n"),
		[]byte("[support]\nhelpline\n[generic]\nshop\n"))
	if err != nil {
		t.Fatal(err)
	}
	if x.Skeleton("oscаr") != "oscar" {
		t.Error("a module remapped a built-in character")
	}
	if x.Skeleton("ցreg") != "greg" || d.Skeleton("ցreg") == "greg" || x.Skeleton("clan") != "dan" {
		t.Errorf("extension not applied: %s %s", x.Skeleton("ցreg"), x.Skeleton("clan"))
	}
	if x.SupportWord("annahelpline") == "" || !x.Generic("shop") || d.Generic("shop") {
		t.Error("words not added")
	}
	if _, err := d.With([]byte("zz ; 61 ; MA\n"), nil); err == nil {
		t.Error("a bad line accepted")
	}
	if _, err := d.With(nil, []byte("[nope]\nx\n")); err == nil {
		t.Error("a bad section accepted")
	}
}
