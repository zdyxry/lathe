package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

var columnFormatPresets = map[string]ColumnFormat{
	"bytes":         {Kind: "scaled_number", Base: 1024, Units: []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}, MaxFractionDigits: 2},
	"decimal_bytes": {Kind: "scaled_number", Base: 1000, Units: []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}, MaxFractionDigits: 2},
	"hz":            {Kind: "scaled_number", Base: 1000, Units: []string{"Hz", "kHz", "MHz", "GHz", "THz", "PHz", "EHz"}, MaxFractionDigits: 2},
	"number":        {Kind: "number", Grouping: true, MaxFractionDigits: 2},
}

// ColumnFormatPreset returns one of the built-in reusable table column
// formats. Callers may merge explicit fields on top of the returned value.
func ColumnFormatPreset(name string) (ColumnFormat, bool) {
	format, ok := columnFormatPresets[name]
	if !ok {
		return ColumnFormat{}, false
	}
	format.Units = slices.Clone(format.Units)
	return format, true
}

// NormalizeColumnFormat expands built-in presets and applies runtime defaults.
func NormalizeColumnFormat(format ColumnFormat) (ColumnFormat, bool) {
	return resolveColumnFormat(format)
}

func decodeNumberPreserving(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func formatColumnValue(value any, format ColumnFormat) (string, bool) {
	format, ok := resolveColumnFormat(format)
	if !ok {
		return "", false
	}
	switch format.Kind {
	case "currency":
		return formatCurrencyColumnValue(value, format)
	case "number":
		return formatNumberColumnValue(value, format)
	case "scaled_number":
		return formatScaledNumberColumnValue(value, format)
	default:
		return "", false
	}
}

func formatCurrencyColumnValue(value any, format ColumnFormat) (string, bool) {
	negative, digits, ok := integerValue(value)
	if !ok {
		return "", false
	}
	if strings.Trim(digits, "0") == "" {
		negative = false
	}
	integer, fraction := scaleDigits(digits, format.SourceScale)
	for len(fraction) > format.MinFractionDigits && strings.HasSuffix(fraction, "0") {
		fraction = fraction[:len(fraction)-1]
	}
	for len(fraction) < format.MinFractionDigits {
		fraction += "0"
	}
	if format.Grouping {
		integer = groupInteger(integer)
	}
	prefix := format.Currency + " "
	if format.Currency == "USD" {
		prefix = "$"
	}
	var result strings.Builder
	if negative {
		result.WriteByte('-')
	}
	result.WriteString(prefix)
	result.WriteString(integer)
	if fraction != "" {
		result.WriteByte('.')
		result.WriteString(fraction)
	}
	return result.String(), true
}

func formatNumberColumnValue(value any, format ColumnFormat) (string, bool) {
	number, ok := numberValue(value)
	if !ok {
		return "", false
	}
	text := formatRat(number, format.MinFractionDigits, format.MaxFractionDigits)
	if format.Grouping {
		text = groupDecimal(text)
	}
	if format.Unit != "" {
		text += " " + format.Unit
	}
	return text, true
}

func formatScaledNumberColumnValue(value any, format ColumnFormat) (string, bool) {
	number, ok := numberValue(value)
	if !ok {
		return "", false
	}
	scale := chooseScale(number, format)
	divisor := scaleDivisor(format.Base, scale)
	scaled := new(big.Rat).Quo(number, new(big.Rat).SetInt(divisor))
	text := formatRat(scaled, format.MinFractionDigits, format.MaxFractionDigits)
	if format.Grouping {
		text = groupDecimal(text)
	}
	unit := format.Units[scale]
	if format.Unit != "" {
		unit = format.Unit
	}
	if unit != "" {
		text += " " + unit
	}
	return text, true
}

func resolveColumnFormat(format ColumnFormat) (ColumnFormat, bool) {
	if preset, ok := ColumnFormatPreset(format.Kind); ok {
		format = mergeColumnFormat(preset, format)
	}
	if format.Kind == "scaled_number" && format.MinFractionDigits == 0 && format.MaxFractionDigits == 0 {
		format.MaxFractionDigits = 2
	}
	if !validColumnFormat(format) {
		return ColumnFormat{}, false
	}
	return format, true
}

func mergeColumnFormat(base, override ColumnFormat) ColumnFormat {
	out := base
	if override.Currency != "" {
		out.Currency = override.Currency
	}
	if override.Unit != "" {
		out.Unit = override.Unit
	}
	if len(override.Units) > 0 {
		out.Units = slices.Clone(override.Units)
	}
	if override.Base != 0 {
		out.Base = override.Base
	}
	if override.SourceScale != 0 {
		out.SourceScale = override.SourceScale
	}
	if override.Grouping {
		out.Grouping = true
	}
	if override.MinFractionDigits != 0 {
		out.MinFractionDigits = override.MinFractionDigits
	}
	if override.MaxFractionDigits != 0 {
		out.MaxFractionDigits = override.MaxFractionDigits
	}
	return out
}

func validColumnFormat(format ColumnFormat) bool {
	switch format.Kind {
	case "currency":
		return validCurrencyColumnFormat(format)
	case "number":
		return validNumberColumnFormat(format)
	case "scaled_number":
		return validScaledNumberColumnFormat(format)
	default:
		return false
	}
}

func validCurrencyColumnFormat(format ColumnFormat) bool {
	if len(format.Currency) != 3 {
		return false
	}
	for _, r := range format.Currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return format.SourceScale >= 0 && format.SourceScale <= 18 &&
		format.MinFractionDigits >= 0 && format.MinFractionDigits <= 18 &&
		format.MaxFractionDigits >= format.MinFractionDigits && format.MaxFractionDigits <= 18 &&
		format.MaxFractionDigits >= format.SourceScale
}

func validNumberColumnFormat(format ColumnFormat) bool {
	return validFractionDigits(format) && validColumnUnit(format.Unit)
}

func validScaledNumberColumnFormat(format ColumnFormat) bool {
	if format.Base < 2 || len(format.Units) == 0 || !validFractionDigits(format) {
		return false
	}
	for _, unit := range format.Units {
		if unit == "" || !validColumnUnit(unit) {
			return false
		}
	}
	return format.Unit == "" || slices.Contains(format.Units, format.Unit)
}

func validColumnUnit(unit string) bool {
	return unit == "" || strings.TrimSpace(unit) == unit && !strings.ContainsAny(unit, "\t\v\f\r\n")
}

func validFractionDigits(format ColumnFormat) bool {
	if format.MinFractionDigits < 0 || format.MinFractionDigits > 18 {
		return false
	}
	if format.MaxFractionDigits < 0 || format.MaxFractionDigits > 18 {
		return false
	}
	return format.MaxFractionDigits >= format.MinFractionDigits
}

func numberValue(value any) (*big.Rat, bool) {
	raw, ok := rawNumber(value)
	if !ok {
		return nil, false
	}
	return parseNumber(raw)
}

func integerValue(value any) (bool, string, bool) {
	raw, ok := rawNumber(value)
	if !ok {
		return false, "", false
	}
	number, ok := parseNumber(raw)
	if !ok || !number.IsInt() {
		return false, "", false
	}
	integer := number.Num()
	return integer.Sign() < 0, new(big.Int).Abs(integer).String(), true
}

func rawNumber(value any) (string, bool) {
	var raw string
	switch value := value.(type) {
	case json.Number:
		raw = value.String()
	case string:
		raw = value
	case int:
		raw = strconv.Itoa(value)
	case int8:
		raw = strconv.FormatInt(int64(value), 10)
	case int16:
		raw = strconv.FormatInt(int64(value), 10)
	case int32:
		raw = strconv.FormatInt(int64(value), 10)
	case int64:
		raw = strconv.FormatInt(value, 10)
	case uint:
		raw = strconv.FormatUint(uint64(value), 10)
	case uint8:
		raw = strconv.FormatUint(uint64(value), 10)
	case uint16:
		raw = strconv.FormatUint(uint64(value), 10)
	case uint32:
		raw = strconv.FormatUint(uint64(value), 10)
	case uint64:
		raw = strconv.FormatUint(value, 10)
	case float32:
		raw = strconv.FormatFloat(float64(value), 'g', -1, 32)
	case float64:
		raw = strconv.FormatFloat(value, 'g', -1, 64)
	default:
		return "", false
	}
	return raw, true
}

func parseNumber(raw string) (*big.Rat, bool) {
	if raw == "" || len(raw) > 4096 || strings.TrimSpace(raw) != raw || !json.Valid([]byte(raw)) {
		return nil, false
	}
	if _, err := strconv.ParseFloat(raw, 64); err != nil {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, false
	}
	return value, true
}

func scaleDigits(digits string, scale int) (string, string) {
	if scale == 0 {
		return digits, ""
	}
	if len(digits) <= scale {
		return "0", strings.Repeat("0", scale-len(digits)) + digits
	}
	return digits[:len(digits)-scale], digits[len(digits)-scale:]
}

func formatRat(value *big.Rat, minFractionDigits, maxFractionDigits int) string {
	text := value.FloatString(maxFractionDigits)
	if maxFractionDigits <= minFractionDigits {
		return text
	}
	integer, fraction, found := strings.Cut(text, ".")
	if !found {
		return text
	}
	for len(fraction) > minFractionDigits && strings.HasSuffix(fraction, "0") {
		fraction = fraction[:len(fraction)-1]
	}
	if fraction == "" {
		return integer
	}
	return integer + "." + fraction
}

func chooseScale(value *big.Rat, format ColumnFormat) int {
	if format.Unit != "" {
		index := slices.Index(format.Units, format.Unit)
		if index >= 0 {
			return index
		}
		return 0
	}
	abs := new(big.Rat).Abs(value)
	divisor := big.NewRat(1, 1)
	base := big.NewRat(int64(format.Base), 1)
	scale := 0
	for scale+1 < len(format.Units) {
		next := new(big.Rat).Mul(divisor, base)
		if abs.Cmp(next) < 0 {
			break
		}
		divisor = next
		scale++
	}
	return scale
}

func scaleDivisor(base, exponent int) *big.Int {
	result := big.NewInt(1)
	factor := big.NewInt(int64(base))
	for range exponent {
		result.Mul(result, factor)
	}
	return result
}

func groupDecimal(value string) string {
	sign := ""
	var ok bool
	if value, ok = strings.CutPrefix(value, "-"); ok {
		sign = "-"
	}
	integer, fraction, found := strings.Cut(value, ".")
	integer = groupInteger(integer)
	if !found {
		return sign + integer
	}
	return sign + integer + "." + fraction
}

func groupInteger(value string) string {
	if len(value) <= 3 {
		return value
	}
	first := len(value) % 3
	if first == 0 {
		first = 3
	}
	var result strings.Builder
	result.Grow(len(value) + len(value)/3)
	result.WriteString(value[:first])
	for index := first; index < len(value); index += 3 {
		result.WriteByte(',')
		result.WriteString(value[index : index+3])
	}
	return result.String()
}
