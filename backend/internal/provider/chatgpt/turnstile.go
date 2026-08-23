package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"time"
)

type orderedMap struct {
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{values: map[string]any{}}
}

func solveTurnstileToken(dx, p string) string {
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		return ""
	}
	var tokenList [][]any
	if err := json.Unmarshal([]byte(xorString(string(decoded), p)), &tokenList); err != nil {
		return ""
	}

	processMap := map[int]any{16: p}
	start := time.Now()
	result := ""
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	toStr := func(value any) string {
		if value == nil {
			return "undefined"
		}
		if s, ok := value.(string); ok {
			special := map[string]string{
				"window.Math":            "[object Math]",
				"window.Reflect":         "[object Reflect]",
				"window.performance":     "[object Performance]",
				"window.localStorage":    "[object Storage]",
				"window.Object":          "function Object() { [native code] }",
				"window.Reflect.set":     "function set() { [native code] }",
				"window.performance.now": "function () { [native code] }",
				"window.Object.create":   "function create() { [native code] }",
				"window.Object.keys":     "function keys() { [native code] }",
				"window.Math.random":     "function random() { [native code] }",
			}
			if specialValue, ok := special[s]; ok {
				return specialValue
			}
			return s
		}
		if list, ok := value.([]string); ok {
			return strings.Join(list, ",")
		}
		return stringValue(value)
	}

	for _, token := range tokenList {
		if len(token) == 0 {
			continue
		}
		op := intValue(token[0])
		switch op {
		case 2:
			if len(token) >= 3 {
				processMap[intValue(token[1])] = token[2]
			}
		case 3:
			if len(token) >= 2 {
				result = base64.StdEncoding.EncodeToString([]byte(toStr(processMap[intValue(token[1])])))
			}
		case 5:
			if len(token) >= 3 {
				e := intValue(token[1])
				t := intValue(token[2])
				cur := processMap[e]
				inc := processMap[t]
				if list, ok := cur.([]any); ok {
					processMap[e] = append(list, inc)
				} else if _, ok := cur.(string); ok {
					processMap[e] = toStr(cur) + toStr(inc)
				} else {
					processMap[e] = "NaN"
				}
			}
		case 6, 24:
			if len(token) >= 4 {
				e := intValue(token[1])
				t := toStr(processMap[intValue(token[2])])
				n := toStr(processMap[intValue(token[3])])
				v := t + "." + n
				if op == 6 && v == "window.document.location" {
					v = "https://chatgpt.com/"
				}
				processMap[e] = v
			}
		case 8:
			if len(token) >= 3 {
				processMap[intValue(token[1])] = processMap[intValue(token[2])]
			}
		case 14:
			if len(token) >= 3 {
				var parsed any
				if err := json.Unmarshal([]byte(toStr(processMap[intValue(token[2])])), &parsed); err == nil {
					processMap[intValue(token[1])] = parsed
				}
			}
		case 15:
			if len(token) >= 3 {
				b, _ := json.Marshal(processMap[intValue(token[2])])
				processMap[intValue(token[1])] = string(b)
			}
		case 17:
			if len(token) >= 3 {
				e := intValue(token[1])
				target := toStr(processMap[intValue(token[2])])
				switch target {
				case "window.performance.now":
					processMap[e] = float64(time.Since(start).Nanoseconds())/1e6 + rng.Float64()
				case "window.Object.create":
					processMap[e] = newOrderedMap()
				case "window.Object.keys":
					processMap[e] = []string{
						"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
						"STATSIG_LOCAL_STORAGE_STABLE_ID",
						"client-correlated-secret",
						"oai/apps/capExpiresAt",
						"oai-did",
						"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
						"UiState.isNavigationCollapsed.1",
					}
				case "window.Math.random":
					processMap[e] = rng.Float64()
				}
			}
		case 18:
			if len(token) >= 2 {
				raw, err := base64.StdEncoding.DecodeString(toStr(processMap[intValue(token[1])]))
				if err == nil {
					processMap[intValue(token[1])] = string(raw)
				}
			}
		case 19:
			if len(token) >= 2 {
				processMap[intValue(token[1])] = base64.StdEncoding.EncodeToString([]byte(toStr(processMap[intValue(token[1])])))
			}
		case 20:
			if len(token) >= 4 {
				if toStr(processMap[intValue(token[1])]) == toStr(processMap[intValue(token[2])]) {
					if intValue(token[3]) == 3 && len(token) >= 5 {
						result = base64.StdEncoding.EncodeToString([]byte(toStr(processMap[intValue(token[4])])))
					}
				}
			}
		}
	}
	return result
}

func xorString(text, key string) string {
	if key == "" {
		return text
	}
	out := make([]rune, 0, len(text))
	keyRunes := []rune(key)
	for i, ch := range text {
		out = append(out, ch^keyRunes[i%len(keyRunes)])
	}
	return string(out)
}
