package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"
)

// Datasource must implement required interfaces. This is important to do
// since otherwise we will only get a not implemented error response from plugin in
// runtime. Datasource instance implements backend.QueryDataHandler,
// backend.CheckHealthHandler.Implementing instancemgmt.InstanceDisposer
// is useful to clean up resources used by previous datasource instance when a new datasource
// instance created upon datasource settings changed.
var (
	_ backend.QueryDataHandler   = (*Neo4JDatasource)(nil)
	_ backend.CheckHealthHandler = (*Neo4JDatasource)(nil)
	_ backend.DataSourceInstanceSettings
	_ instancemgmt.InstanceDisposer = (*Neo4JDatasource)(nil)
)

const (
	DATASOURCE_UID string = "DATASOURCE_UID"
	ERROR          string = "err"
)

// datasource which can respond to data queries and reports its health.
type Neo4JDatasource struct {
	id       string
	settings neo4JSettings
	driver   neo4j.DriverWithContext
}

// creates a new datasource instance.
func NewNeo4JDatasource(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	id := uuid.New().String()
	log.DefaultLogger.Debug("Create Datasource", DATASOURCE_UID, id)
	neo4JSettings, err := unmarshalDataSourceSettings(settings)
	if err != nil {
		errorMsg := "can not deserialize DataSource settings"
		log.DefaultLogger.Error(errorMsg, ERROR, err.Error())
		return nil, errors.New(errorMsg)
	}

	authToken := neo4j.NoAuth()
	if neo4JSettings.Username != "" && neo4JSettings.Password != "" {
		authToken = neo4j.BasicAuth(neo4JSettings.Username, neo4JSettings.Password, "")
	}

	driver, err := neo4j.NewDriverWithContext(neo4JSettings.Url, authToken)
	if err != nil {
		return nil, err
	}

	return &Neo4JDatasource{
		id:       id,
		settings: neo4JSettings,
		driver:   driver,
	}, nil
}

// Dispose here tells plugin SDK that plugin wants to clean up resources when a new instance
// created. As soon as datasource settings change detected by SDK old datasource instance will
// be disposed and a new one will be created using factory function.
func (d *Neo4JDatasource) Dispose() {
	// Clean up datasource instance resources.
	log.DefaultLogger.Info("Dispose Datasource", DATASOURCE_UID, d.id)
	defer d.driver.Close(context.Background())
}

// QueryData handles multiple queries and returns multiple responses.
// req contains the queries []DataQuery (where each query contains RefID as a unique identifier).
// The QueryDataResponse contains a map of RefID to the response for each query, and each response
// contains Frames ([]*Frame).
func (d *Neo4JDatasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	log.DefaultLogger.Debug("QueryData called", DATASOURCE_UID, d.id)

	// create response struct
	response := backend.NewQueryDataResponse()

	// loop over queries and execute them individually.
	for _, q := range req.Queries {

		var res backend.DataResponse

		// Unmarshal the JSON into our queryModel.
		var neo4JQuery neo4JQuery
		err := json.Unmarshal(q.JSON, &neo4JQuery)
		if err != nil {
			res.Error = err
			response.Responses[q.RefID] = res
			continue
		}

		neo4JQuery.RefID = q.RefID
		neo4JQuery.QueryType = q.QueryType
		neo4JQuery.Interval = q.Interval
		neo4JQuery.MaxDataPoints = q.MaxDataPoints
		neo4JQuery.TimeRange = q.TimeRange

		res, err = d.query(ctx, neo4JQuery)
		if err != nil {
			res.Error = err
		}

		if res.Error != nil {
			log.DefaultLogger.Error("Error in query", ERROR, res.Error)
		}

		response.Responses[q.RefID] = res
	}

	return response, nil
}

func (d *Neo4JDatasource) query(ctx context.Context, query neo4JQuery) (backend.DataResponse, error) {
	log.DefaultLogger.Debug("Execute Cypher Query: '"+query.CypherQuery+"'", DATASOURCE_UID, d.id)

	response := backend.DataResponse{}

	session := d.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: d.settings.Database, AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.Run(ctx, query.CypherQuery, map[string]interface{}{})

	if err != nil {
		errMsg := "InternalError!"
		switch err.(type) {
		default:
			return response, err
		case *neo4j.ConnectivityError:
			errMsg = "ConnectivityError: Can not connect to specified url."
		}

		log.DefaultLogger.Error(errMsg, ERROR, err.Error())
		return response, errors.New(errMsg + " Please review log for more details.")
	}
	// return appropriate format according to the choosen format(nodegraph or table)
	if query.Format == "nodegraph" {
		return toGraphResponse(ctx, result)
	} else {
		return toDataResponse(ctx, result)
	}
}

func toDataResponse(ctx context.Context, result neo4j.ResultWithContext) (backend.DataResponse, error) {
	response := backend.DataResponse{}

	keys, err := result.Keys()
	if err != nil {
		return response, err
	}

	// create data frame response.
	frame := data.NewFrame("response")

	var allRecords, _ = result.Collect(ctx)

	// infer data type per column and define frame for it
	for columnNr, columnName := range keys {
		var typ interface{}
		if len(allRecords) > 0 {
			typ = getTypeArray(allRecords[0], columnNr)
		} else {
			typ = getTypeArray(nil, columnNr)
		}

		if typ == nil {
			log.DefaultLogger.Debug("Could not infer type from first columnNr, because value was nil. Trying next rows")

			for i := 1; i < len(allRecords) && typ == nil; i++ {
				typ = getTypeArray(allRecords[i], columnNr)
			}
		}

		if typ == nil {
			log.DefaultLogger.Debug("After looking at all rows, type is still nil. Assigning string-type as default")
			typ = []*string{}
		}

		frame.Fields = append(frame.Fields,
			data.NewField(columnName, nil, typ),
		)
	}

	// iterate through rows and append frame of values to result
	for _, currentRecord := range allRecords {
		values := currentRecord.Values
		vals := make([]interface{}, len(frame.Fields))
		for col, v := range values {
			val := toValue(v)
			vals[col] = val
		}
		frame.AppendRow(vals...)
	}

	// add the frames to the response.
	response.Frames = append(response.Frames, frame)
	return response, nil
}

func createGraphDataFrame(name string, typ interface{}, metaFields []string, allRecords []*neo4j.Record) (*data.Frame, map[string]int, int) {

	// anonymous function to create dataframe with string fields
	createStringFieldList := func(fields []string) []*data.Field {
		var dataFieldList []*data.Field
		for _, field := range fields {
			dataFieldList = append(dataFieldList, data.NewField(field, nil, []*string{}))
		}
		return dataFieldList
	}

	fieldList := createStringFieldList(metaFields)

	propMap := make(map[string]int)
	propIndex := len(metaFields)

	for _, currentRecord := range allRecords {
		values := currentRecord.Values
		for _, v := range values {

			var isType bool
			var props map[string]any
			switch typ.(type) {
			default:
				continue
			case dbtype.Node:
				_, isType = v.(dbtype.Node)
				if isType {
					props = v.(dbtype.Node).Props
				}
			case dbtype.Relationship:
				_, isType = v.(dbtype.Relationship)
				if isType {
					props = v.(dbtype.Relationship).Props
				}
			}

			if isType {
				for name := range props {
					// check if prop already exists.
					if _, exists := propMap[name]; !exists {
						propMap[name] = 0
					}
				}
			}
		}
	}

	var propNames []string
	for name := range propMap {
		propNames = append(propNames, name)
	}

	sort.Strings(propNames)

	for _, name := range propNames {
		fieldList = append(fieldList, data.NewField("detail__"+name, nil, []*string{}))
		propMap[name] = propIndex
		propIndex = propIndex + 1
	}

	return data.NewFrame(name, fieldList...), propMap, propIndex

}

// Return customized response for node graph panel
func toGraphResponse(ctx context.Context, result neo4j.ResultWithContext) (backend.DataResponse, error) {
	response := backend.DataResponse{}

	// Check if query has any keys.
	_, err := result.Keys()
	if err != nil {
		return response, err
	}

	var allRecords, _ = result.Collect(ctx)

	// a map of Id to empty string to prevent insert duplicate nodes in the dataframe
	nodeIdMap := make(map[string]string)

	// https://grafana.com/docs/grafana/latest/panels-visualizations/visualizations/node-graph/#nodes-data-frame-structure
	nodesFrame, nodesPropMap, nodesRowLen := createGraphDataFrame("nodes", dbtype.Node{}, []string{"id", "title", "detail__labels"}, allRecords)

	// https://grafana.com/docs/grafana/latest/panels-visualizations/visualizations/node-graph/#edges-data-frame-structure
	edgesFrame, edgesPropMap, edgesRowLen := createGraphDataFrame("edges", dbtype.Relationship{}, []string{"id", "source", "target", "mainStat"}, allRecords)

	// iterate through rows and append nodes to frame
	for _, currentRecord := range allRecords {
		values := currentRecord.Values
		for _, v := range values {
			node, isNode := v.(dbtype.Node)
			if isNode {
				// check if this Node was already added.
				if _, exists := nodeIdMap[node.ElementId]; !exists {

					firstLabel := ""
					if len(node.Labels) > 0 {
						firstLabel = node.Labels[0]
					}

					nodeIdMap[node.ElementId] = ""

					row := make([]interface{}, nodesRowLen)
					row[0] = &node.ElementId
					row[1] = &firstLabel
					if len(node.Labels) > 1 {
						row[2] = asJson(node.Labels)
					}
					for name, value := range node.Props {
						propIndex := nodesPropMap[name]
						row[propIndex] = asJson(value)
					}

					nodesFrame.AppendRow(row...)
				}
			}
		}
	}

	// iterate through rows and append edges to frame
	for _, currentRecord := range allRecords {
		values := currentRecord.Values
		for _, v := range values {
			edge, isEdge := v.(dbtype.Relationship)

			// check if Start and End ElementId exists.
			_, startExists := nodeIdMap[edge.StartElementId]
			_, endExists := nodeIdMap[edge.EndElementId]

			if isEdge && startExists && endExists {
				row := make([]interface{}, edgesRowLen)
				row[0] = &edge.ElementId
				row[1] = &edge.StartElementId
				row[2] = &edge.EndElementId
				row[3] = &edge.Type
				for name, value := range edge.Props {
					propIndex := edgesPropMap[name]
					row[propIndex] = asJson(value)
				}

				edgesFrame.AppendRow(row...)
			}
		}
	}

	// Set Preffered Visualization to nodegraph for both data frames
	m := data.FrameMeta{PreferredVisualization: "nodeGraph"}
	nodesFrame = nodesFrame.SetMeta(&m)
	edgesFrame = edgesFrame.SetMeta(&m)

	// add the frames to the response.
	response.Frames = append(response.Frames, nodesFrame, edgesFrame)
	return response, nil
}

// CheckHealth handles health checks sent from Grafana to the plugin.
// The main use case for these health checks is the test button on the
// datasource configuration page which allows users to verify that
// a datasource is working as expected.
func (d *Neo4JDatasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	return d.checkHealth(ctx)
}

func (d *Neo4JDatasource) checkHealth(ctx context.Context) (*backend.CheckHealthResult, error) {
	log.DefaultLogger.Debug("CheckHealth called", DATASOURCE_UID, d.id)

	err := d.driver.VerifyConnectivity(ctx)

	// Some errs are not tackled by VerifyConnectivity
	if err == nil {
		neo4JQuery := neo4JQuery{
			CypherQuery: "Match(a) return a limit 1",
		}

		_, err = d.query(ctx, neo4JQuery)
	}

	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: err.Error(),
		}, nil
	}

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Data source is working",
	}, nil
}

func unmarshalDataSourceSettings(dSIset backend.DataSourceInstanceSettings) (neo4JSettings, error) {
	// Unmarshal the JSON into our settings Model.
	var neo4JSettings neo4JSettings
	err := json.Unmarshal(dSIset.JSONData, &neo4JSettings)
	if err != nil {
		return neo4JSettings, err

	}

	if decryptedPassword, exists := dSIset.DecryptedSecureJSONData["password"]; exists {
		neo4JSettings.Password = decryptedPassword
	}

	return neo4JSettings, nil
}

func getTypeArray(record *neo4j.Record, idx int) interface{} {
	if record == nil {
		return []*string{}
	}

	typ := record.Values[idx]

	return getTypeArrayByVal(typ)
}

// https://github.com/neo4j/neo4j-go-driver#value-types
func getTypeArrayByVal(typ any) interface{} {
	switch typ.(type) {
	case int64:
		return []*int64{}
	case float64:
		return []*float64{}
	case bool:
		return []*bool{}
	case time.Time, dbtype.Date, dbtype.Time, dbtype.LocalTime, dbtype.LocalDateTime:
		return []*time.Time{}
	case nil:
		return nil
	default:
		return []*string{}
	}
}

// https://github.com/neo4j/neo4j-go-driver#value-types
func toValue(val interface{}) interface{} {
	if val == nil {
		return nil
	}
	switch t := val.(type) {
	case string:
		return &t
	case int64:
		return &t
	case bool:
		return &t
	case float64:
		return &t
	case time.Time:
		return &t
	case dbtype.Date:
		val := t.Time()
		return &val
	case dbtype.Time:
		val := t.Time()
		return &val
	case dbtype.LocalTime:
		val := t.Time()
		return &val
	case dbtype.LocalDateTime:
		val := t.Time()
		return &val
	case dbtype.Duration:
		val := t.String()
		return &val
	default:
		return asJson(val)
	}
}

func asJson(val interface{}) *string {
	r, err := json.Marshal(val)
	if err != nil {
		log.DefaultLogger.Info("Json marshalling failed ", ERROR, err)
	}
	res := string(r)
	return &res
}

type neo4JQuery struct {
	RefID string

	// QueryType is an optional identifier for the type of query.
	// It can be used to distinguish different types of queries.
	QueryType string

	// MaxDataPoints is the maximum number of datapoints that should be returned from a time series query.
	MaxDataPoints int64

	// Interval is the suggested duration between time points in a time series query.
	Interval time.Duration

	// TimeRange is the Start and End of the query as sent by the frontend.
	TimeRange backend.TimeRange

	CypherQuery string `json:"cypherQuery"`
	Format      string `json:"Format"`
}

type neo4JSettings struct {
	Url      string `json:"url"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}
