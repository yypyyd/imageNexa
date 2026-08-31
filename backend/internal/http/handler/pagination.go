package handler

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func parseInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

// pageParams reads limit/offset query params with the given default page size.
// A limit of 0 (or negative) is treated as "return everything" so callers that
// need the full filtered set (e.g. bulk actions) can pass ?limit=0.
func pageParams(c *gin.Context, defLimit int) (limit, offset int) {
	limit = parseInt(c.Query("limit"), defLimit)
	if limit < 0 {
		limit = 0
	}
	offset = parseInt(c.Query("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// pageSlice returns items[offset : offset+limit], clamped to the slice bounds.
// limit <= 0 means "from offset to the end". Always returns a non-nil slice so
// it JSON-encodes as [] rather than null.
func pageSlice[T any](items []T, limit, offset int) []T {
	n := len(items)
	if offset >= n {
		return []T{}
	}
	end := n
	if limit > 0 && offset+limit < n {
		end = offset + limit
	}
	return items[offset:end]
}
