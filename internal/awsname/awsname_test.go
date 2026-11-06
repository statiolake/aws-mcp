package awsname

import "testing"

func TestCLIOperation(t *testing.T) {
	cases := map[string]string{
		"DescribeInstances":          "describe-instances",
		"describe_instances":         "describe-instances",
		"describe-instances":         "describe-instances",
		"DescribeDBInstances":        "describe-db-instances",
		"describe_db_instances":      "describe-db-instances",
		"ListObjectsV2":              "list-objects-v2",
		"list_objects_v2":            "list-objects-v2",
		"GetCallerIdentity":          "get-caller-identity",
		"ListTagsForResource":        "list-tags-for-resource",
		"DescribeVpcEndpoints":       "describe-vpc-endpoints",
		"ListUserARNs":               "list-user-arns",
		"AssignIpv6Addresses":        "assign-ipv6-addresses",
		"GetBucketACL":               "get-bucket-acl",
		"Scan":                       "scan",
		"BatchGetItem":               "batch-get-item",
		"DescribeDBClusterSnapshots": "describe-db-cluster-snapshots",
		"ExecuteStatement":           "execute-statement",
	}
	for in, want := range cases {
		if got := CLIOperation(in); got != want {
			t.Errorf("CLIOperation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCLIService(t *testing.T) {
	cases := map[string]string{"s3": "s3api", "S3": "s3api", "ec2": "ec2", "config": "configservice",
		"codedeploy": "deploy", "rds-data": "rds-data", "s3api": "s3api"}
	for in, want := range cases {
		if got := CLIService(in); got != want {
			t.Errorf("CLIService(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPascalOperation(t *testing.T) {
	cases := map[string]string{"describe_instances": "DescribeInstances", "DescribeDBInstances": "DescribeDBInstances",
		"get-object": "GetObject"}
	for in, want := range cases {
		if got := PascalOperation(in); got != want {
			t.Errorf("PascalOperation(%q) = %q, want %q", in, got, want)
		}
	}
}
