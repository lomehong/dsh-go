// Embedded tz database so IANA zone resolution is deterministic on hosts
// without a system zoneinfo.
package schedule

import _ "time/tzdata"
