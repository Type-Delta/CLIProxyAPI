package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQueryAcceptsV1V2OperationsAndNamedRanges(t *testing.T) {
	v1 := `{"schema_version":1,"operation":"summary","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","time_zone":"UTC"}`
	if _, err := ParseQuery([]byte(v1)); err != nil {
		t.Fatalf("v1 query rejected: %v", err)
	}

	for _, operation := range []string{"activity", "analysis"} {
		window := ""
		if operation == "activity" {
			window = `,"window":"day"`
		}
		query := `{"schema_version":2,"operation":"` + operation + `","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","time_zone":"UTC"` + window + `}`
		if _, err := ParseQuery([]byte(query)); err != nil {
			t.Fatalf("v2 %s query rejected: %v", operation, err)
		}
	}

	namedRange := `{"schema_version":2,"operation":"summary","range":{"preset":"last_n_days","n":7,"time_zone":"Asia/Bangkok"}}`
	if _, err := ParseQuery([]byte(namedRange)); err != nil {
		t.Fatalf("v2 named range query rejected: %v", err)
	}
}

func TestCalendarNamedRangesRejectRollingAndCustomFields(t *testing.T) {
	for _, preset := range []string{"today", "yesterday", "this_week", "prev_week", "this_month", "prev_month", "this_year", "prev_year"} {
		t.Run(preset, func(t *testing.T) {
			rangeRequest := RangeRequest{Preset: preset, TimeZone: "America/St_Johns"}
			if err := rangeRequest.Validate(); err != nil {
				t.Fatal(err)
			}
			rangeRequest.N = 1
			if err := rangeRequest.Validate(); err == nil {
				t.Fatal("calendar preset accepted n")
			}
			for name, value := range map[string]time.Time{
				"start": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				"end":   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			} {
				rangeRequest = RangeRequest{Preset: preset, TimeZone: "America/St_Johns"}
				if name == "start" {
					rangeRequest.Start = value
				} else {
					rangeRequest.End = value
				}
				if err := rangeRequest.Validate(); err == nil {
					t.Fatalf("calendar preset accepted %s", name)
				}
			}
		})
	}
}

func TestActivityWindowValidation(t *testing.T) {
	for _, window := range []string{"day", "week", "month", "year"} {
		query := `{"schema_version":2,"operation":"activity","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","time_zone":"UTC","window":"` + window + `"}`
		parsed, err := ParseQuery([]byte(query))
		if err != nil {
			t.Fatalf("activity window %q rejected: %v", window, err)
		}
		if err := parsed.Validate(); err != nil {
			t.Fatalf("activity window %q failed validation: %v", window, err)
		}
	}

	invalid := `{"schema_version":2,"operation":"activity","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","time_zone":"UTC","window":"quarter"}`
	parsed, err := ParseQuery([]byte(invalid))
	if err == nil {
		err = parsed.Validate()
	}
	if err == nil {
		t.Fatal("invalid activity window was accepted")
	}
	rangeLess := `{"schema_version":2,"operation":"activity","window":"day"}`
	parsed, err = ParseQuery([]byte(rangeLess))
	if err == nil {
		err = parsed.Validate()
	}
	if err == nil {
		t.Fatal("range-less activity query was accepted")
	}
}

func TestV2EventsValidateResultErrorClassAndSourceFilters(t *testing.T) {
	for _, result := range []string{"success", "failure"} {
		query := `{"schema_version":2,"operation":"events","start":"2026-01-01T00:00:00Z","end":"2026-01-02T00:00:00Z","time_zone":"UTC","filters":{"result":"` + result + `","error_class":["rate_limit"],"source":["import-42"]}}`
		parsed, err := ParseQuery([]byte(query))
		if err != nil {
			t.Fatalf("event result %q rejected: %v", result, err)
		}
		if err := parsed.Validate(); err != nil {
			t.Fatalf("event result %q failed validation: %v", result, err)
		}
	}
}

func TestSummaryMarshalsFrozenV2Fields(t *testing.T) {
	assertJSONFields(t, Summary{}, []string{
		"succeeded", "failed", "success_rate", "requests_per_minute", "tokens_per_minute",
		"cache_read_rate", "range_days", "avg_requests_per_day", "avg_tokens_per_day",
		"avg_known_cost_usd_per_day", "price_coverage_complete",
	})
}

func TestAnalysisSectionsAreIndependentlyNullableWithPartialMeta(t *testing.T) {
	analysisType := reflect.TypeOf(Analysis{})
	analysisValue := reflect.New(analysisType).Elem()
	for _, section := range []string{"series_by_category", "model_by_time", "latency", "cost_components", "key_model_matrix"} {
		field, ok := jsonField(analysisType, section)
		if !ok {
			t.Fatalf("analysis is missing %q", section)
		}
		if field.Type.Kind() != reflect.Pointer {
			t.Fatalf("analysis section %q type = %v, want pointer for independent nullability", section, field.Type)
		}
		sectionType := field.Type.Elem()
		meta, ok := jsonField(sectionType, "meta")
		if !ok {
			t.Fatalf("analysis section %q is missing meta", section)
		}
		partial, ok := jsonField(derefType(meta.Type), "partial")
		if !ok || partial.Type.Kind() != reflect.Bool {
			t.Fatalf("analysis section %q meta is missing boolean partial", section)
		}

		if !field.IsExported() {
			t.Fatalf("analysis section %q is not exported", section)
		}
		sectionValue := reflect.New(sectionType)
		setJSONBool(sectionValue.Elem(), "meta", "partial", true)
		analysisValue.FieldByIndex(field.Index).Set(sectionValue)
	}

	encoded, err := json.Marshal(analysisValue.Interface())
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("decode analysis: %v", err)
	}
	for _, section := range []string{"series_by_category", "model_by_time", "latency", "cost_components", "key_model_matrix"} {
		raw, ok := object[section]
		if !ok || string(raw) == "null" {
			t.Fatalf("analysis section %q was not independently serialized", section)
		}
		var sectionObject map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sectionObject); err != nil {
			t.Fatalf("decode analysis section %q: %v", section, err)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(sectionObject["meta"], &meta); err != nil {
			t.Fatalf("decode analysis section %q meta: %v", section, err)
		}
		if string(meta["partial"]) != "true" {
			t.Fatalf("analysis section %q meta.partial = %s, want true", section, meta["partial"])
		}
	}

	nilEncoded, err := json.Marshal(Analysis{})
	if err != nil {
		t.Fatalf("marshal nil analysis: %v", err)
	}
	var nilObject map[string]json.RawMessage
	if err := json.Unmarshal(nilEncoded, &nilObject); err != nil {
		t.Fatalf("decode nil analysis: %v", err)
	}
	for _, section := range []string{"series_by_category", "model_by_time", "latency", "cost_components", "key_model_matrix"} {
		if string(nilObject[section]) != "null" {
			t.Fatalf("nil analysis section %q = %s, want null", section, nilObject[section])
		}
	}
}

func TestKeyIdentityMarshalsLifetimeBounds(t *testing.T) {
	assertJSONFields(t, KeyIdentity{}, []string{"lifetime_first_activity_at", "lifetime_last_activity_at"})
}

func TestProviderCredentialDTOIsSanitizedAndContainsQuotaFields(t *testing.T) {
	credentialType := reflect.TypeOf(ProviderCredential{})
	assertJSONFields(t, ProviderCredential{}, []string{
		"credential_id", "provider", "auth_type", "status", "requests", "failed",
		"last_error_class", "last_error_at", "quota", "observed_at",
	})
	quota, ok := jsonField(credentialType, "quota")
	if !ok {
		t.Fatal("provider credential is missing quota")
	}
	assertJSONFields(t, reflect.New(derefType(quota.Type)).Elem().Interface(), []string{"limit", "used", "remaining", "resets_at"})

	encoded, err := json.Marshal(ProviderCredential{})
	if err != nil {
		t.Fatalf("marshal provider credential: %v", err)
	}
	output := string(encoded)
	for _, forbidden := range []string{"auth_index", "raw-auth-id", "authorization", "api_key"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("provider credential JSON leaked %q: %s", forbidden, output)
		}
	}
}

func assertJSONFields(t *testing.T, value any, required []string) {
	t.Helper()
	typ := reflect.TypeOf(value)
	for _, name := range required {
		field, ok := jsonField(typ, name)
		if !ok {
			t.Errorf("%v is missing JSON field %q", typ, name)
			continue
		}
		if field.Tag.Get("json") == "" {
			t.Errorf("%v field %q has no JSON tag", typ, name)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %v: %v", typ, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("decode %v: %v", typ, err)
	}
	for _, name := range required {
		if _, ok := object[name]; !ok {
			t.Errorf("%v JSON omitted field %q", typ, name)
		}
	}
}

func jsonField(typ reflect.Type, name string) (reflect.StructField, bool) {
	typ = derefType(typ)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func derefType(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func setJSONBool(value reflect.Value, parentName, name string, want bool) {
	parent := value.FieldByIndex(mustJSONField(value.Type(), parentName).Index)
	for parent.Kind() == reflect.Pointer {
		if parent.IsNil() {
			parent.Set(reflect.New(parent.Type().Elem()))
		}
		parent = parent.Elem()
	}
	field := parent.FieldByIndex(mustJSONField(parent.Type(), name).Index)
	field.SetBool(want)
}

func mustJSONField(typ reflect.Type, name string) reflect.StructField {
	field, ok := jsonField(typ, name)
	if !ok {
		panic("missing JSON field " + name)
	}
	return field
}
