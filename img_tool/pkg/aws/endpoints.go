package aws

import "strings"

// DNSSuffix returns the DNS suffix of the AWS partition a region belongs to.
func DNSSuffix(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "amazonaws.com.cn"
	case strings.HasPrefix(region, "eusc-"):
		return "amazonaws.eu"
	default:
		return "amazonaws.com"
	}
}
