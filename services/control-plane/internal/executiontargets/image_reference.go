package executiontargets

import "strings"

func immutableImageDigest(reference string) string {
	reference = strings.TrimSpace(reference)
	separator := strings.LastIndex(reference, "@")
	if separator <= 0 || separator == len(reference)-1 {
		return ""
	}
	digest := reference[separator+1:]
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	for _, character := range digest[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return ""
		}
	}
	return digest
}
