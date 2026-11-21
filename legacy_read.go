package notifications

import "slices"

// These closed enums match the last published v0.1.2 inbox response. Keep the
// default HTTP list, count and mark-all queries within the same compatibility
// scope. New clients can opt in to the expanded contract.
var legacyTitleKeys = []string{
	"notifications.interaction.reply",
	"notifications.interaction.like",
	"notifications.content.approved",
	"notifications.content.rejected",
	"notifications.content.removed",
	"notifications.resource_removal.approved",
	"notifications.resource_removal.rejected",
	"notifications.report.approved",
	"notifications.report.rejected",
	"notifications.student_verification.approved",
	"notifications.student_verification.rejected",
	"notifications.identity_verification.approved",
	"notifications.identity_verification.rejected",
	"notifications.verification_revocation.approved",
	"notifications.verification_revocation.rejected",
	"notifications.verification_deletion.approved",
	"notifications.verification_deletion.rejected",
	"notifications.teacher_verification.approved",
	"notifications.teacher_verification.rejected",
	"notifications.student_verification.revoked",
	"notifications.system.notice",
	"notifications.system.security",
	"notifications.system.account",
}

var legacyTargetKinds = []string{
	"review",
	"comment",
	"resource",
	"resource_removal_request",
	"report",
	"verification_application",
	"teacher_application",
	"verification_credential",
	"verification_removal_request",
	"verification_operation",
}

func LegacyTitleKeys() []string { return slices.Clone(legacyTitleKeys) }

func LegacyTargetKinds() []string { return slices.Clone(legacyTargetKinds) }
