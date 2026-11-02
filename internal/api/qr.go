package api

import (
	qrcode "github.com/skip2/go-qrcode"
)

// qrPNG renders a join URL as a scannable PNG.
//
// Medium error correction is the sweet spot for a code that will be projected
// onto a screen or printed: enough redundancy to survive glare and a bad camera
// angle, without inflating the module count so far that phones at the back of a
// room struggle to resolve it.
func qrPNG(url string, size int) ([]byte, error) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	q.DisableBorder = false
	return q.PNG(size)
}
