package archive

import "testing"

func TestSkillUseRetainsMultipleHashesForSameName(t *testing.T) {
	var metadata Metadata
	deriveSkills(SourceBundle{}, []SkillUse{
		{Name: "review", SHA256: "aaa", Evidence: SkillUseEvidenceNativeInvocation},
		{Name: "review", SHA256: "bbb", Evidence: SkillUseEvidenceNativeInvocation},
		{Name: "review", Evidence: SkillUseEvidenceReadInference},
	}, &metadata)
	if len(metadata.SkillsUsed) != 2 || metadata.SkillsUsed[0].SHA256 != "aaa" || metadata.SkillsUsed[1].SHA256 != "bbb" {
		t.Fatalf("lost skill version: %#v", metadata.SkillsUsed)
	}
}
