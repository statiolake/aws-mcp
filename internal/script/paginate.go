package script

import (
	"bytes"
	"encoding/json"
)

// tokenSpec names the continuation token(s) of a paginated operation: the
// response members in out are copied to the request members in in.
type tokenSpec struct {
	in, out []string
	// truncFlag, if set, is a boolean response member that must be true for
	// another page to exist (IAM's IsTruncated).
	truncFlag string
}

// knownTokens covers the continuation-token conventions of AWS APIs, in
// priority order. Botocore's paginator models are not available to the CLI
// backend, so paginate recognises tokens by name; scripts can override with
// _input_token/_output_token.
var knownTokens = []tokenSpec{
	{in: []string{"NextToken"}, out: []string{"NextToken"}},
	{in: []string{"nextToken"}, out: []string{"nextToken"}},
	{in: []string{"ContinuationToken"}, out: []string{"NextContinuationToken"}}, // S3 ListObjectsV2
	{in: []string{"KeyMarker", "VersionIdMarker"}, out: []string{"NextKeyMarker", "NextVersionIdMarker"}},
	{in: []string{"KeyMarker", "UploadIdMarker"}, out: []string{"NextKeyMarker", "NextUploadIdMarker"}},
	{in: []string{"StartRecordName", "StartRecordType", "StartRecordIdentifier"},
		out: []string{"NextRecordName", "NextRecordType", "NextRecordIdentifier"}}, // Route 53
	{in: []string{"StartRecordName", "StartRecordType"}, out: []string{"NextRecordName", "NextRecordType"}},
	{in: []string{"Marker"}, out: []string{"NextMarker"}},
	{in: []string{"PageToken"}, out: []string{"NextPageToken"}},
	{in: []string{"pageToken"}, out: []string{"nextPageToken"}},
	{in: []string{"ExclusiveStartKey"}, out: []string{"LastEvaluatedKey"}},             // DynamoDB Scan/Query
	{in: []string{"ExclusiveStartTableName"}, out: []string{"LastEvaluatedTableName"}}, // DynamoDB ListTables
	{in: []string{"Marker"}, out: []string{"Marker"}, truncFlag: "IsTruncated"},        // IAM
	{in: []string{"Marker"}, out: []string{"Marker"}},                                  // RDS, ELB, ...
	{in: []string{"nextToken"}, out: []string{"nextForwardToken"}},                     // CloudWatch Logs GetLogEvents
}

// detectToken finds the continuation token in a response, or nil if the
// response is the last page.
func detectToken(out map[string]json.RawMessage) *tokenSpec {
	if t, ok := out["IsTruncated"]; ok && bytes.Equal(bytes.TrimSpace(t), []byte("false")) {
		return nil
	}
	for i := range knownTokens {
		s := &knownTokens[i]
		if _, ok := s.values(out); ok {
			return s
		}
	}
	return nil
}

// values returns the JSON token values from a response if all are present and
// non-empty.
func (s *tokenSpec) values(out map[string]json.RawMessage) ([]string, bool) {
	if s.truncFlag != "" {
		t, ok := out[s.truncFlag]
		if !ok || !bytes.Equal(bytes.TrimSpace(t), []byte("true")) {
			return nil, false
		}
	}
	vals := make([]string, len(s.out))
	for i, k := range s.out {
		v, ok := out[k]
		if !ok {
			// A shorter spec later in knownTokens covers responses where an
			// optional member (NextRecordIdentifier) is absent.
			return nil, false
		}
		t := bytes.TrimSpace(v)
		if len(t) == 0 || string(t) == "null" || string(t) == `""` || string(t) == "{}" {
			return nil, false
		}
		vals[i] = string(t)
	}
	return vals, true
}
