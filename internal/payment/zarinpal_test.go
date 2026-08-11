package payment

import "testing"

func TestTomanToRial(t *testing.T) {
	got, err := TomanToRial(129900)
	if err != nil {
		t.Fatalf("TomanToRial returned error: %v", err)
	}
	if got != 1299000 {
		t.Fatalf("TomanToRial = %d, want 1299000", got)
	}

	if _, err := TomanToRial(-1); err == nil {
		t.Fatal("TomanToRial(-1) returned nil error")
	}
}
