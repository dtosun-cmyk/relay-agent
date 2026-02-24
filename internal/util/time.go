package util

import "time"

// Turkey timezone (UTC+3)
var TurkeyLocation *time.Location

func init() {
	var err error
	TurkeyLocation, err = time.LoadLocation("Europe/Istanbul")
	if err != nil {
		// Fallback to fixed offset if timezone data not available
		TurkeyLocation = time.FixedZone("Turkey", 3*60*60)
	}
}

// NowTurkey returns current time in Turkey timezone
// The time is returned with UTC location but the actual time value is Turkey time
// This ensures MongoDB stores the Turkey time value without converting to UTC
func NowTurkey() time.Time {
	now := time.Now().In(TurkeyLocation)
	// Create a new time with same values but in UTC location
	// This tricks MongoDB into storing Turkey time as-is
	return time.Date(
		now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second(), now.Nanosecond(),
		time.UTC,
	)
}

// ToTurkey converts a time to Turkey timezone
func ToTurkey(t time.Time) time.Time {
	return t.In(TurkeyLocation)
}
