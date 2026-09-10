package model

import (
	"strings"
	"time"
)

const DolaPublicVideoModel = "dola-seedance-2.5"

const DolaDailyVideoBucket = "dola.video.daily"
const DolaDailyVideoLimit = 2

// Dola uses a local UTC accounting day. Upstream quota refusals remain
// authoritative; the local rollover does not prove an upstream reset.
func DolaVideoBucketAt(now time.Time) string {
	return DolaDailyVideoBucket + ":" + now.UTC().Format(time.DateOnly)
}

func DolaVideoBucketDay(key string) (time.Time, bool) {
	day, err := time.Parse(time.DateOnly, strings.TrimPrefix(key, DolaDailyVideoBucket+":"))
	return day, err == nil && key == DolaVideoBucketAt(day)
}
