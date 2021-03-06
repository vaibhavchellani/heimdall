package types

// query endpoints supported by the auth Querier
const (
	QueryRecord     = "record"
	QueryRecordList = "record-list"
)

// QueryRecordParams defines the params for querying accounts.
type QueryRecordParams struct {
	RecordID uint64
}

// NewQueryRecordParams creates a new instance of QueryRecordParams.
func NewQueryRecordParams(recordID uint64) QueryRecordParams {
	return QueryRecordParams{RecordID: recordID}
}
