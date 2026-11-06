package manifest

import "testing"

func TestVercmp(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0}, {"1.0", "2.0", -1}, {"2.0.1", "2.0", 1}, {"5.5p1", "5.5p2", -1},
		{"10xyz", "10.1xyz", -1}, {"xyz10", "xyz10.1", -1}, {"1.0a", "1.0", 1},
		{"1.0~rc1", "1.0", -1}, {"1.0~rc1", "1.0~rc2", -1}, {"1.0^", "1.0", 1},
		{"1.0^git1", "1.0", 1}, {"1.0^git1", "1.01", -1}, {"a", "1", -1}, {"1", "a", 1},
		{"1.05", "1.5", 0}, {"26.05.4", "26.05.10", -1}, {"2.el9", "10.el9", -1},
	}
	for _, c := range cases {
		if got := Vercmp(c.a, c.b); got != c.want {
			t.Errorf("Vercmp(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if CompareEVR(1, "1.0", "1", 0, "9.0", "1") != 1 {
		t.Error("epoch wins")
	}
	if CompareNVR("ucx-1.22.1-1.el9", "ucx-1.22.0-2.el9") != 1 {
		t.Error("CompareNVR")
	}
}
