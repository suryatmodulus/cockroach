// Code generated by "stringer"; DO NOT EDIT.

package clusterversion

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[V21_1-0]
	_ = x[Start21_1PLUS-1]
	_ = x[Start21_2-2]
	_ = x[AutoSpanConfigReconciliationJob-3]
	_ = x[DefaultPrivileges-4]
	_ = x[ZonesTableForSecondaryTenants-5]
	_ = x[UseKeyEncodeForHashShardedIndexes-6]
	_ = x[DatabasePlacementPolicy-7]
	_ = x[GeneratedAsIdentity-8]
	_ = x[OnUpdateExpressions-9]
	_ = x[SpanConfigurationsTable-10]
	_ = x[BoundedStaleness-11]
	_ = x[DateAndIntervalStyle-12]
	_ = x[TenantUsageSingleConsumptionColumn-13]
	_ = x[SQLStatsTables-14]
	_ = x[SQLStatsCompactionScheduledJob-15]
	_ = x[V21_2-16]
	_ = x[Start22_1-17]
	_ = x[AvoidDrainingNames-18]
	_ = x[DrainingNamesMigration-19]
	_ = x[TraceIDDoesntImplyStructuredRecording-20]
	_ = x[AlterSystemTableStatisticsAddAvgSizeCol-21]
	_ = x[AlterSystemStmtDiagReqs-22]
	_ = x[MVCCAddSSTable-23]
	_ = x[InsertPublicSchemaNamespaceEntryOnRestore-24]
	_ = x[UnsplitRangesInAsyncGCJobs-25]
	_ = x[ValidateGrantOption-26]
	_ = x[PebbleFormatBlockPropertyCollector-27]
	_ = x[ProbeRequest-28]
	_ = x[SelectRPCsTakeTracingInfoInband-29]
	_ = x[PreSeedTenantSpanConfigs-30]
	_ = x[SeedTenantSpanConfigs-31]
	_ = x[PublicSchemasWithDescriptors-32]
}

const _Key_name = "V21_1Start21_1PLUSStart21_2AutoSpanConfigReconciliationJobDefaultPrivilegesZonesTableForSecondaryTenantsUseKeyEncodeForHashShardedIndexesDatabasePlacementPolicyGeneratedAsIdentityOnUpdateExpressionsSpanConfigurationsTableBoundedStalenessDateAndIntervalStyleTenantUsageSingleConsumptionColumnSQLStatsTablesSQLStatsCompactionScheduledJobV21_2Start22_1AvoidDrainingNamesDrainingNamesMigrationTraceIDDoesntImplyStructuredRecordingAlterSystemTableStatisticsAddAvgSizeColAlterSystemStmtDiagReqsMVCCAddSSTableInsertPublicSchemaNamespaceEntryOnRestoreUnsplitRangesInAsyncGCJobsValidateGrantOptionPebbleFormatBlockPropertyCollectorProbeRequestSelectRPCsTakeTracingInfoInbandPreSeedTenantSpanConfigsSeedTenantSpanConfigsPublicSchemasWithDescriptors"

var _Key_index = [...]uint16{0, 5, 18, 27, 58, 75, 104, 137, 160, 179, 198, 221, 237, 257, 291, 305, 335, 340, 349, 367, 389, 426, 465, 488, 502, 543, 569, 588, 622, 634, 665, 689, 710, 738}

func (i Key) String() string {
	if i < 0 || i >= Key(len(_Key_index)-1) {
		return "Key(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _Key_name[_Key_index[i]:_Key_index[i+1]]
}
