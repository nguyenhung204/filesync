package cli

import "testing"

// D2 mode decision: the dispatcher outcome alone decides
// whether the process runs as a service, as a console app, or
// gives up.
func TestServeModeForDispatch(t *testing.T) {
	cases := []struct {
		name    string
		errCode uint32
		want    serveMode
	}{
		{"success", 0, serveModeService},
		{"not started by SCM", 1063, serveModeConsole},
		{"access denied", 5, serveModeFail},
		{"service database locked", 1055, serveModeFail},
		{"unknown error", 9999, serveModeFail},
	}
	for _, c := range cases {
		if got := serveModeForDispatch(c.errCode); got != c.want {
			t.Errorf("%s: serveModeForDispatch(%d) = %v, want %v", c.name, c.errCode, got, c.want)
		}
	}
	if errFailedServiceControllerConnect != 1063 {
		t.Errorf("errFailedServiceControllerConnect = %d, want 1063", errFailedServiceControllerConnect)
	}
}

// D5 bit math: QuickEdit (0x0040) is cleared, extended flags
// (0x0080) is set, and unrelated bits (mouse input, insert
// mode, processed input) survive untouched.
func TestConsoleModeWithoutQuickEdit(t *testing.T) {
	cases := []struct {
		name string
		in   uint32
		want uint32
	}{
		{"typical default mode", 0x01E7, 0x01A7}, // 0x80 already set, 0x40 cleared
		{"quickedit only", 0x0040, 0x0080},
		{"already off", 0x0080, 0x0080},
		{"zero", 0x0000, 0x0080},
		{"all bits", 0xFFFF, 0xFFFF &^ 0x0040},
	}
	for _, c := range cases {
		got := consoleModeWithoutQuickEdit(c.in)
		if got != c.want {
			t.Errorf("%s: consoleModeWithoutQuickEdit(%#04x) = %#04x, want %#04x", c.name, c.in, got, c.want)
		}
		if got&enableQuickEditMode != 0 {
			t.Errorf("%s: QuickEdit bit still set in %#04x", c.name, got)
		}
		if got&enableExtendedFlags == 0 {
			t.Errorf("%s: extended flags bit missing in %#04x", c.name, got)
		}
	}
}
