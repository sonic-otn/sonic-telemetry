package common_utils

func IsTargetDb( target string) bool {
	isDbClient := false
	dbTargetSupported := []string {"APPL_DB", "ASIC_DB" , "COUNTERS_DB", "LOGLEVEL_DB",
		                           "CONFIG_DB", "PFC_WD_DB", "FLEX_COUNTER_DB", "STATE_DB"}

	for _, name := range  dbTargetSupported {
		if  target ==  name {
			isDbClient = true
			return isDbClient
		}
	}

	return isDbClient
}
