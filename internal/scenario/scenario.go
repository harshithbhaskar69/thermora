// Package scenario supplies deterministic local inputs, not live weather or market data.
package scenario

import (
	"math"
	"time"
)

var Names = []string{"constant", "winter", "negative-price", "missing-price", "missing-forecast"}

func Code(name string) (uint16, bool) {
	for i, n := range Names {
		if n == name {
			return uint16(i), true
		}
	}
	return 0, false
}

// Inputs are indexed by UTC intervals; local DST changes never duplicate or omit an instant.
func Inputs(code uint16, at time.Time) (price, forecast *float64, outdoor float64) {
	hour := at.UTC().Hour()
	outdoor = -3 + 3*math.Sin(2*math.Pi*(float64(hour)-8)/24)
	if code == 0 {
		outdoor = -3
	}
	weather := outdoor
	forecast = &weather
	p := .12
	switch {
	case hour < 5:
		p = .05
	case hour >= 6 && hour < 10:
		p = .32
	case hour >= 16 && hour < 20:
		p = .38
	case hour >= 22:
		p = .07
	}
	if code == 0 {
		p = .15
	}
	if code == 2 && hour < 5 {
		p = -.08
	}
	price = &p
	if code == 3 && hour == 7 {
		price = nil
	}
	if code == 4 && hour == 7 {
		forecast = nil
	}
	return price, forecast, outdoor
}
