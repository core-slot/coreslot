package memsample

import "testing"

func TestMeminfo(t *testing.T) {
	const text = "MemTotal:       16303412 kB\nMemAvailable: 9000000 kB\n"
	for key, want := range map[string]int64{"MemTotal": 16303412, "MemAvailable": 9000000} {
		if got, ok := meminfo(text, key); !ok || got != want {
			t.Errorf("meminfo(%s) = %d, %v; want %d", key, got, ok, want)
		}
	}
	if got, ok := meminfo(text, "Foo"); ok {
		t.Errorf("meminfo(Foo) = %d, true; want not found", got)
	}
}
