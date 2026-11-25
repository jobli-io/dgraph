/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package types

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unsafe"

	"github.com/pkg/errors"
)

// BytesAsFloatArray(encoded) converts encoded into a []float32.
// If len(encoded) % 4 is not 0, it will ignore any trailing
// bytes, and simply convert 4 bytes at a time to generate the
// float64 entries.
// Current implementation assuming littleEndian encoding
func BytesAsFloatArray(encoded []byte) []float32 {
	resultLen := len(encoded) / 4
	if resultLen == 0 {
		return []float32{}
	}
	retVal := make([]float32, resultLen)
	for i := range resultLen {
		retVal[i] = *(*float32)(unsafe.Pointer(&encoded[0]))
		encoded = encoded[4:]
	}
	return retVal
}

// FloatArrayAsBytes(v) will create a byte array encoding
// v using LittleEndian format. This is sort of the inverse
// of BytesAsFloatArray, but note that we can always be successful
// converting to bytes, but the inverse is not feasible.
func FloatArrayAsBytes(v []float32) []byte {
	retVal := make([]byte, 4*len(v))
	offset := retVal
	for i := range v {
		bits := math.Float32bits(v[i])
		binary.LittleEndian.PutUint32(offset, bits)
		offset = offset[4:]
	}
	return retVal
}

func FloatArrayAsString(v []float32) string {
	var sb strings.Builder

	sb.WriteRune('[')
	for i := range v {
		sb.WriteString(strconv.FormatFloat(float64(v[i]), 'f', -1, 32))
		if i != len(v)-1 {
			sb.WriteRune(',')
		}
	}
	sb.WriteRune(']')
	return sb.String()
}

// StringArrayAsBytes encodes v using an offset-table binary format:
//
//	[count: uint32 LE][endOffset_0 .. endOffset_{N-1}: uint32 LE][concatenated UTF-8 bytes]
//
// An empty slice []string{} produces 4 zero bytes (count = 0), distinguishing an explicit
// empty list from an unset/nil value.
func StringArrayAsBytes(v []string) []byte {
	totalBytes := 0
	for _, s := range v {
		totalBytes += len(s)
	}
	headerSize := 4 * (1 + len(v))
	retVal := make([]byte, headerSize+totalBytes)
	binary.LittleEndian.PutUint32(retVal[0:4], uint32(len(v)))
	endOffset := 0
	for i, s := range v {
		endOffset += len(s)
		binary.LittleEndian.PutUint32(retVal[4*(i+1):4*(i+2)], uint32(endOffset))
	}
	payload := retVal[headerSize:]
	pos := 0
	for _, s := range v {
		pos += copy(payload[pos:], s)
	}
	return retVal
}

// ValidateStringArrayBytes checks whether encoded is a structurally valid StringArrayAsBytes buffer.
func ValidateStringArrayBytes(encoded []byte) error {
	if len(encoded) == 0 {
		return nil
	}
	if len(encoded) < 4 {
		return errors.Errorf("invalid data for vector of strings: truncated header")
	}
	count := int(int64(binary.LittleEndian.Uint32(encoded[0:4])))
	if count > (len(encoded)-4)/4 {
		return errors.Errorf("invalid data for vector of strings: truncated offset table")
	}
	headerSize := 4 * (1 + count)
	payloadLen := len(encoded) - headerSize
	prevEnd := 0
	for i := range count {
		end := int(int64(binary.LittleEndian.Uint32(encoded[4*(i+1) : 4*(i+2)])))
		if end < prevEnd || end > payloadLen {
			return errors.Errorf("invalid data for vector of strings: corrupt offset %d", end)
		}
		prevEnd = end
	}
	if prevEnd != payloadLen {
		return errors.Errorf("invalid data for vector of strings: trailing bytes")
	}
	return nil
}

// BytesAsStringArray decodes an offset-table encoded byte slice into []string
// using a single string allocation for the entire payload.
func BytesAsStringArray(encoded []byte) []string {
	if len(encoded) < 4 {
		return []string{}
	}
	count := int(int64(binary.LittleEndian.Uint32(encoded[0:4])))
	if count == 0 || count > (len(encoded)-4)/4 {
		return []string{}
	}
	headerSize := 4 * (1 + count)
	payload := string(encoded[headerSize:])
	retVal := make([]string, count)
	prevEnd := 0
	for i := range count {
		end := int(int64(binary.LittleEndian.Uint32(encoded[4*(i+1) : 4*(i+2)])))
		if end < prevEnd || end > len(payload) {
			return []string{}
		}
		retVal[i] = payload[prevEnd:end]
		prevEnd = end
	}
	return retVal
}

// CountVString returns the number of elements in an encoded stringvector in O(1) time.
func CountVString(encoded []byte) int {
	if len(encoded) < 4 {
		return 0
	}
	count := int(int64(binary.LittleEndian.Uint32(encoded[0:4])))
	if count > (len(encoded)-4)/4 {
		return 0
	}
	return count
}

// StringArrayAsString formats a []string as a JSON string array without HTML escaping.
func StringArrayAsString(v []string) string {
	if v == nil {
		v = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "[]"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// TypeForValue tries to determine the most likely type based on a value. We only want to use this
// function when there's no schema type and no suggested storage type.
// Returns the guessed type or DefaultID if it couldn't be determined.
// If retval is non-nil, the parsed value is returned, useful in conjunction with ObjectValue().
func TypeForValue(v []byte) (TypeID, interface{}) {
	s := string(v)
	switch {
	case v == nil || s == "":
		break

	// Possible boolean. Specific to "true" or "false".
	case s[0] == 't', s[0] == 'T', s[0] == 'f', s[0] == 'F':
		var b bool
		// XXX: we dont use ParseBool here because it considers 't' and 'f' as values.
		switch s {
		case "true", "TRUE", "True":
			b = true
			return BoolID, b
		case "false", "FALSE", "False":
			return BoolID, b
		}

	// Possible datetime. Unfortunately, year-only value will fallthrough as int.
	case checkDateTime(s):
		if t, err := ParseTime(s); err == nil {
			return DateTimeID, t
		}

	// Possible int.
	case checkInt(s):
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return IntID, i
		}

	// Possible float.
	case checkFloat(s):
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return FloatID, f
		}
	}

	// TODO: Consider looking for vector type (vfloat). Not clear
	//       about this, because the natural encoding as
	//       "[ num, num, ... num ]" my look like a standard list type.
	return DefaultID, nil
}

func isSign(d byte) bool {
	return d == '-' || d == '+'
}

func isDigit(d byte) bool {
	return d >= '0' && d <= '9'
}

func checkInt(s string) bool {
	if isSign(s[0]) && len(s) > 1 {
		s = s[1:]
	}
	return isDigit(s[0]) && !strings.ContainsAny(s[1:], ".Ee")
}

func checkFloat(s string) bool {
	if isSign(s[0]) && len(s) > 1 {
		s = s[1:]
	}
	if s[0] == '.' && len(s) > 1 {
		// .012 is totally legit
		return isDigit(s[1])
	}
	return isDigit(s[0]) && strings.ContainsAny(s[1:], ".Ee")
}

func checkDateTime(s string) bool {
	if len(s) < 5 {
		return false
	}
	return isDigit(s[0]) && isDigit(s[1]) && isDigit(s[2]) && isDigit(s[3]) && s[4] == '-'
}
