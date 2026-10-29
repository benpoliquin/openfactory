package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"openfactory/internal/model"
)

func decodeObservation(data []byte, spec model.ExperimentObservationSpec) (any, error) {
	var value any
	if err := model.StrictDecode(data, &value); err != nil {
		return nil, fmt.Errorf("invalid or ambiguous JSON output: %w", err)
	}
	if spec.JSONPointer != "" {
		if !strings.HasPrefix(spec.JSONPointer, "/") {
			return nil, errors.New("JSON pointer must start with slash")
		}
		for _, encoded := range strings.Split(spec.JSONPointer[1:], "/") {
			key, err := pointerKey(encoded)
			if err != nil {
				return nil, err
			}
			switch object := value.(type) {
			case map[string]any:
				var ok bool
				value, ok = object[key]
				if !ok {
					return nil, errors.New("JSON pointer field is absent")
				}
			case []any:
				if key == "" || (len(key) > 1 && key[0] == '0') {
					return nil, errors.New("invalid JSON pointer array index")
				}
				index, err := strconv.Atoi(key)
				if err != nil || index < 0 || index >= len(object) {
					return nil, errors.New("JSON pointer array index is absent")
				}
				value = object[index]
			default:
				return nil, errors.New("JSON pointer traverses scalar")
			}
		}
	}
	switch spec.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return nil, errors.New("observation is not boolean")
		}
	case "string":
		if _, ok := value.(string); !ok {
			return nil, errors.New("observation is not string")
		}
	case "number", "integer":
		number, ok := value.(json.Number)
		if !ok {
			return nil, errors.New("observation is not numeric")
		}
		rational, ok := boundedNumber(number)
		if !ok {
			return nil, errors.New("observation number exceeds bounded numeric representation")
		}
		if spec.Type == "integer" && !rational.IsInt() {
			return nil, errors.New("observation is not integer")
		}
	default:
		return nil, errors.New("unsupported observation type")
	}
	return value, nil
}
func pointerKey(encoded string) (string, error) {
	var key strings.Builder
	for i := 0; i < len(encoded); i++ {
		if encoded[i] != '~' {
			key.WriteByte(encoded[i])
			continue
		}
		i++
		if i >= len(encoded) {
			return "", errors.New("invalid JSON pointer escape")
		}
		switch encoded[i] {
		case '0':
			key.WriteByte('~')
		case '1':
			key.WriteByte('/')
		default:
			return "", errors.New("invalid JSON pointer escape")
		}
	}
	return key.String(), nil
}
func evaluatePredicates(contract model.ExperimentContract, observations []model.ExperimentObservation) (bool, error) {
	byName := map[string]model.ExperimentObservation{}
	for _, observation := range observations {
		byName[observation.Name] = observation
	}
	accepted := true
	for _, predicate := range contract.Acceptance {
		observation, ok := byName[predicate.Observation]
		if !ok {
			return false, errors.New("required observation is missing")
		}
		if predicate.Unit != observation.Unit {
			return false, errors.New("predicate unit differs from observation unit")
		}
		var actual, expected any
		if err := model.StrictDecode(observation.Value, &actual); err != nil {
			return false, err
		}
		if err := model.StrictDecode(predicate.Value, &expected); err != nil {
			return false, err
		}
		match, err := predicateMatch(actual, expected, predicate.Operator)
		if err != nil {
			return false, err
		}
		accepted = accepted && match
	}
	return accepted, nil
}
func predicateMatch(actual, expected any, operator string) (bool, error) {
	if !boundedNumericValues(actual) || !boundedNumericValues(expected) {
		return false, errors.New("numeric predicate exceeds bounded representation")
	}
	switch operator {
	case "eq":
		return equalJSON(actual, expected), nil
	case "ne":
		return !equalJSON(actual, expected), nil
	case "in":
		members, ok := expected.([]any)
		if !ok {
			return false, errors.New("membership predicate requires an array")
		}
		for _, member := range members {
			if equalJSON(actual, member) {
				return true, nil
			}
		}
		return false, nil
	case "lt", "le", "gt", "ge":
		left, leftOK := actual.(json.Number)
		right, rightOK := expected.(json.Number)
		if !leftOK || !rightOK {
			return false, errors.New("order predicates require numeric values")
		}
		a, ok := new(big.Rat).SetString(string(left))
		if !ok {
			return false, errors.New("invalid numeric observation")
		}
		b, ok := new(big.Rat).SetString(string(right))
		if !ok {
			return false, errors.New("invalid numeric predicate")
		}
		comparison := a.Cmp(b)
		switch operator {
		case "lt":
			return comparison < 0, nil
		case "le":
			return comparison <= 0, nil
		case "gt":
			return comparison > 0, nil
		default:
			return comparison >= 0, nil
		}
	}
	return false, errors.New("unsupported predicate")
}
func equalJSON(left, right any) bool {
	a, ok := left.(json.Number)
	if ok {
		b, ok := right.(json.Number)
		if !ok {
			return false
		}
		ar, ok := new(big.Rat).SetString(string(a))
		if !ok {
			return false
		}
		br, ok := new(big.Rat).SetString(string(b))
		return ok && ar.Cmp(br) == 0
	}
	return reflect.DeepEqual(left, right)
}

// Bound exact arithmetic before allocation. This is a numeric representation
// capability limit, not a silent float64 rounding or an invented observation.
func boundedNumber(number json.Number) (*big.Rat, bool) {
	if !numericWithinBounds(number) {
		return nil, false
	}
	return new(big.Rat).SetString(string(number))
}
func numericWithinBounds(number json.Number) bool {
	text := string(number)
	if len(text) > 1024 {
		return false
	}
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(text[index+1:])
		if err != nil || exponent < -4096 || exponent > 4096 {
			return false
		}
	}
	return true
}
func boundedNumericValues(value any) bool {
	switch item := value.(type) {
	case json.Number:
		return numericWithinBounds(item)
	case []any:
		for _, member := range item {
			if !boundedNumericValues(member) {
				return false
			}
		}
	case map[string]any:
		return false
	}
	return true
}
