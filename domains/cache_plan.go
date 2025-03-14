package domains

type CachePlan struct {
	Queries []*CachePlanQuery `yaml:"queries"`
}

type CachePlanQuery struct {
	*CachePlanQueryBase
	Select *CachePlanSelectQuery
	Update *CachePlanUpdateQuery
	Delete *CachePlanDeleteQuery
	Insert *CachePlanInsertQuery
}

type CachePlanQueryBase struct {
	Query string             `yaml:"query"`
	Type  CachePlanQueryType `yaml:"type"`
}

type CachePlanQueryType string

const (
	CachePlanQueryType_SELECT CachePlanQueryType = "select"
	CachePlanQueryType_UPDATE CachePlanQueryType = "update"
	CachePlanQueryType_DELETE CachePlanQueryType = "delete"
	CachePlanQueryType_INSERT CachePlanQueryType = "insert"
)

type CachePlanPlaceholder struct {
	Index int  `yaml:"index"`
	Extra bool `yaml:"extra,omitempty"`
}

type CachePlanCondition struct {
	Column      string                `yaml:"column"`
	Operator    CachePlanOperatorEnum `yaml:"operator,omitempty"`
	Placeholder CachePlanPlaceholder  `yaml:"placeholder"`
}

type CachePlanOperatorEnum string

const (
	CachePlanOperator_EQ CachePlanOperatorEnum = "eq"
	CachePlanOperator_IN CachePlanOperatorEnum = "in"
)

type CachePlanOrder struct {
	Column string             `yaml:"column"`
	Order  CachePlanOrderEnum `yaml:"order"`
}

type CachePlanOrderEnum string

const (
	CachePlanOrder_ASC  CachePlanOrderEnum = "asc"
	CachePlanOrder_DESC CachePlanOrderEnum = "desc"
)

type CachePlanSelectQuery struct {
	Table      string               `yaml:"table,omitempty"`
	Cache      bool                 `yaml:"cache"`
	Targets    []string             `yaml:"targets,omitempty"`
	Conditions []CachePlanCondition `yaml:"conditions,omitempty"`
	Orders     []CachePlanOrder     `yaml:"orders,omitempty"`
}

type CachePlanUpdateTarget struct {
	Column      string               `yaml:"column"`
	Placeholder CachePlanPlaceholder `yaml:"placeholder"`
}

type CachePlanUpdateQuery struct {
	Table      string                  `yaml:"table"`
	Targets    []CachePlanUpdateTarget `yaml:"targets"`
	Conditions []CachePlanCondition    `yaml:"conditions,omitempty"`
	Orders     []CachePlanOrder        `yaml:"orders,omitempty"`
}

type CachePlanDeleteQuery struct {
	Table      string               `yaml:"table"`
	Conditions []CachePlanCondition `yaml:"conditions,omitempty"`
	Orders     []CachePlanOrder     `yaml:"orders,omitempty"`
}

type CachePlanInsertQuery struct {
	Table   string   `yaml:"table"`
	Columns []string `yaml:"columns"`
}
