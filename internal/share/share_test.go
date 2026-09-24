package share

import "testing"

func TestSeatsRoundUpAndStayInRange(t *testing.T) {
	cases := []struct {
		f    float64
		n    int
		want int
	}{{0.25, 8, 2}, {0.3, 8, 3}, {0.01, 8, 1}, {1, 8, 8}, {0.5, 4, 2}, {0.125, 8, 1}}
	for _, c := range cases {
		got, err := Seats(c.f, c.n)
		if err != nil || got != c.want {
			t.Fatalf("Seats(%g,%d) = %d, %v; want %d", c.f, c.n, got, err, c.want)
		}
	}
	for _, bad := range []float64{0, -0.1, 1.01} {
		if _, err := Seats(bad, 8); err == nil {
			t.Fatalf("Seats(%g) accepted", bad)
		}
	}
}

func TestGrantedAndCaps(t *testing.T) {
	if Granted(3, 8) != 0.375 {
		t.Fatal("granted")
	}
	if ComputePercent(3, 8) != 38 || ComputePercent(8, 8) != 100 {
		t.Fatal("compute percent")
	}
	if DefaultMemMiB(2, 8, 81920) != 20480 {
		t.Fatal("default memory")
	}
}

func TestParseMem(t *testing.T) {
	for in, want := range map[string]int{"20G": 20480, "512M": 512, "1.5G": 1536, "2048": 2048, "20GiB": 20480, "300mb": 300} {
		got, err := ParseMem(in)
		if err != nil || got != want {
			t.Fatalf("ParseMem(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lots", "-1G"} {
		if _, err := ParseMem(bad); err == nil {
			t.Fatalf("ParseMem(%q) accepted", bad)
		}
	}
	if FormatMem(20480) != "20G" || FormatMem(1536) != "1.5G" || FormatMem(300) != "300M" {
		t.Fatal("format")
	}
}
