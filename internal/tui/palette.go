package tui

// Role is a semantic slot in the palette. Renderers ask for roles, never for
// hex, so the light and dark steps swap in exactly one place.
type Role int

// Ink and chrome.
const (
	RoleText Role = iota
	RoleSecondary
	RoleMuted // axis labels, gridlines, legends: must recede
	RoleAxis

	// RoleSeries is categorical slot 1. puffy's line charts plot a single
	// series, so slot 1 is the only categorical hue in play and the chart title
	// carries the identity instead of a legend.
	RoleSeries

	// Status roles. Reserved: they mean good/warning/serious/critical and are
	// never reused as a series colour. Every use is paired with a glyph or a
	// label, so meaning never rests on hue alone.
	RoleGood
	RoleWarning
	RoleSerious
	RoleCritical

	// Latency ordinal ramp, blue, light to dark. Five classes is the readable
	// ceiling for colour-coded bins.
	RoleLat1
	RoleLat2
	RoleLat3
	RoleLat4
	RoleLat5

	// Loss ordinal ramp, orange. Two magnitude encodings share the screen, so
	// the second takes the next categorical hue as its own one-hue ramp.
	RoleLoss1
	RoleLoss2
	RoleLoss3
	RoleLoss4
	RoleLoss5
)

// LatRamp and LossRamp expose the ramps in order, for legends and for binning.
var (
	LatRamp  = []Role{RoleLat1, RoleLat2, RoleLat3, RoleLat4, RoleLat5}
	LossRamp = []Role{RoleLoss1, RoleLoss2, RoleLoss3, RoleLoss4, RoleLoss5}
)

// The ramps below are the documented blue sequential ramp and an orange ramp
// built on the same lightness steps at orange's hue. Both were checked with the
// palette validator as ordinal ramps against each mode's surface: single hue,
// monotone lightness, adjacent gaps >= 0.06, and a lightest step that still
// clears 2:1 against the surface it sits on. Do not hand-edit a step without
// re-running that check - the light end is what breaks first.
var lightHex = map[Role]string{
	RoleText:      "#0b0b0b",
	RoleSecondary: "#52514e",
	RoleMuted:     "#898781",
	RoleAxis:      "#c3c2b7",
	RoleSeries:    "#2a78d6",

	RoleGood:     "#0ca30c",
	RoleWarning:  "#fab219",
	RoleSerious:  "#ec835a",
	RoleCritical: "#d03b3b",

	RoleLat1: "#86b6ef", // blue 250
	RoleLat2: "#5598e7", // blue 350
	RoleLat3: "#2a78d6", // blue 450
	RoleLat4: "#1c5cab", // blue 550
	RoleLat5: "#104281", // blue 650

	RoleLoss1: "#ec9b7e", // orange 250
	RoleLoss2: "#e07249", // orange 350
	RoleLoss3: "#ca4804", // orange 450
	RoleLoss4: "#a13400", // orange 550
	RoleLoss5: "#782200", // orange 650
}

var darkHex = map[Role]string{
	RoleText:      "#ffffff",
	RoleSecondary: "#c3c2b7",
	RoleMuted:     "#898781",
	RoleAxis:      "#383835",
	RoleSeries:    "#3987e5",

	RoleGood:     "#0ca30c",
	RoleWarning:  "#fab219",
	RoleSerious:  "#ec835a",
	RoleCritical: "#d03b3b",

	// The dark ramps use the same five validated steps as the light ones, one
	// notch lighter at each end, but assigned to the magnitude classes in the
	// opposite order. A sequential scale has to let "near zero" recede into the
	// surface; on a dark surface that is the dark end, so magnitude has to climb
	// towards lighter. Running them light-to-dark here would make the quietest
	// cells the loudest ones on screen.
	RoleLat1: "#184f95", // blue 600
	RoleLat2: "#256abf", // blue 500
	RoleLat3: "#3987e5", // blue 400
	RoleLat4: "#6da7ec", // blue 300
	RoleLat5: "#9ec5f4", // blue 200

	RoleLoss1: "#8c2c00", // orange 600
	RoleLoss2: "#b43f02", // orange 500
	RoleLoss3: "#da5821", // orange 400
	RoleLoss4: "#e78663", // orange 300
	RoleLoss5: "#f2af97", // orange 200
}

func (c Color) hex(r Role) string {
	m := darkHex
	if !c.dark {
		m = lightHex
	}
	if h, ok := m[r]; ok {
		return h
	}
	return "#898781"
}

// bin maps a 0..1 magnitude onto a five-class ramp. Zero is deliberately not a
// class: an empty cell is drawn as surface, so the palest step always means
// "some, but barely" rather than "none".
func bin(frac float64, ramp []Role) Role {
	switch {
	case frac <= 0.05:
		return ramp[0]
	case frac <= 0.25:
		return ramp[1]
	case frac <= 0.50:
		return ramp[2]
	case frac <= 0.80:
		return ramp[3]
	default:
		return ramp[4]
	}
}
