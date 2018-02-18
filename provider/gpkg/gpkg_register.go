// +build cgo

package gpkg

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/terranodo/tegola/geom"
	"github.com/terranodo/tegola/internal/log"
	"github.com/terranodo/tegola/provider"
	"github.com/terranodo/tegola/util/dict"
)

func init() {
	provider.Register(Name, NewTileProvider, Cleanup)
}

// Metadata for feature tables in gpkg database
type featureTableDetails struct {
	idFieldname   string
	geomFieldname string
	geomType      geom.Geometry
	srid          uint64
	bbox          geom.BoundingBox
}

// Creates a config instance of the type NewTileProvider() requires including all available feature
//    tables in the gpkg at 'gpkgPath'.
func AutoConfig(gpkgPath string) (map[string]interface{}, error) {
	// Get all feature tables
	db, err := sql.Open("sqlite3", gpkgPath)
	if err != nil {
		return nil, err
	}

	ftMetaData, err := featureTableMetaData(db)
	if err != nil {
		return nil, err
	}

	// Handle table config creation in consistent order to facilitate testing
	tnames := make([]string, len(ftMetaData))
	i := 0
	for tname := range ftMetaData {
		tnames[i] = tname
		i++
	}
	sort.Strings(tnames)

	conf := make(map[string]interface{})
	conf["name"] = "autoconfd_gpkg"
	conf["type"] = "gpkg"
	conf["filepath"] = gpkgPath
	conf["layers"] = make([]map[string]interface{}, len(tnames))
	for i, tablename := range tnames {
		lconf := make(map[string]interface{})
		lconf["name"] = tablename
		lconf["tablename"] = tablename
		lconf["id_fieldname"] = ftMetaData[tablename].idFieldname
		conf["layers"].([]map[string]interface{})[i] = lconf
	}

	return conf, nil
}

// Collect meta data about all feature tables in opened gpkg.
func featureTableMetaData(gpkg *sql.DB) (map[string]featureTableDetails, error) {
	//	this query is used to read the metadata from the gpkg_contents, gpkg_geometry_columns, and
	// sqlite_master tables for tables that store geographic features.
	qtext := `
		SELECT
			c.table_name, c.min_x, c.min_y, c.max_x, c.max_y, c.srs_id, gc.column_name, gc.geometry_type_name, sm.sql
		FROM
			gpkg_contents c JOIN gpkg_geometry_columns gc ON c.table_name == gc.table_name JOIN sqlite_master sm ON c.table_name = sm.tbl_name
		WHERE
			c.data_type = 'features' AND sm.type = 'table';`

	rows, err := gpkg.Query(qtext)
	if err != nil {
		log.Errorf("error during query: %v - %v", qtext, err)
		return nil, err
	}
	defer rows.Close()

	//	container for tracking metadata for each table with a geometry
	geomTableDetails := make(map[string]featureTableDetails)

	// Find the primary key from the table's creation sql.
	pkPattern := `"(.+)" .*?PRIMARY KEY`
	pkFinder := regexp.MustCompile(pkPattern)

	//	iterate each row extracting meta data about each table
	for rows.Next() {
		var tablename, geomCol, geomType, tableSql sql.NullString
		var minX, minY, maxX, maxY sql.NullFloat64
		var srid sql.NullInt64

		if err = rows.Scan(&tablename, &minX, &minY, &maxX, &maxY, &srid, &geomCol, &geomType, &tableSql); err != nil {
			return nil, err
		}
		if !tableSql.Valid {
			return nil, fmt.Errorf("invalid sql for table '%v'", tablename)
		}

		// extract the table's primary key from it's creation sql
		matches := pkFinder.FindStringSubmatch(tableSql.String)
		pkCol := matches[1]

		// map the returned geom type to a tegola geom type
		tg, err := geomNameToGeom(geomType.String)
		if err != nil {
			log.Errorf("error mapping geom type (%v): %v", geomType, err)
			return nil, err
		}

		geomTableDetails[tablename.String] = featureTableDetails{
			idFieldname:   pkCol,
			geomFieldname: geomCol.String,
			geomType:      tg,
			srid:          uint64(srid.Int64),
			//	the extent of the layer's features
			bbox: geom.BoundingBox{{minX.Float64, minY.Float64}, {maxX.Float64, maxY.Float64}},
		}
	}

	return geomTableDetails, nil
}

func NewTileProvider(config map[string]interface{}) (provider.Tiler, error) {
	//	parse our config
	m := dict.M(config)

	filepath, err := m.String(ConfigKeyFilePath, nil)
	if err != nil {
		return nil, err
	}
	if filepath == "" {
		return nil, ErrInvalidFilePath{filepath}
	}

	db, err := sql.Open("sqlite3", filepath)
	if err != nil {
		return nil, err
	}

	geomTableDetails, err := featureTableMetaData(db)
	if err != nil {
		return nil, err
	}

	p := Provider{
		Filepath: filepath,
		layers:   make(map[string]Layer),
		db:       db,
	}

	layers, ok := config[ConfigKeyLayers].([]map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected %v to be a []map[string]interface{}", ConfigKeyLayers)
	}

	lyrsSeen := make(map[string]int)
	for i, v := range layers {

		layerConf := dict.M(v)

		layerName, err := layerConf.String(ConfigKeyLayerName, nil)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) we got the following error trying to get the layer's name field: %v", i, err)
		}
		if layerName == "" {
			return nil, ErrMissingLayerName
		}

		//	check if we have already seen this layer
		if j, ok := lyrsSeen[layerName]; ok {
			return nil, fmt.Errorf("layer name (%v) is duplicated in both layer %v and layer %v", layerName, i, j)
		}
		lyrsSeen[layerName] = i

		if layerConf[ConfigKeyTableName] == nil && layerConf[ConfigKeySQL] == nil {
			return nil, errors.New("'tablename' or 'sql' is required for a feature's config")
		}

		if layerConf[ConfigKeyTableName] != nil && layerConf[ConfigKeySQL] != nil {
			return nil, errors.New("'tablename' or 'sql' is required for a feature's config. you have both")
		}

		idFieldname := DefaultIDFieldName
		idFieldname, err = layerConf.String(ConfigKeyGeomIDField, &idFieldname)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v : %v", i, layerName, err)
		}

		tagFieldnames, err := layerConf.StringSlice(ConfigKeyFields)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v %v field had the following error: %v", i, layerName, ConfigKeyFields, err)
		}

		//	layer container. will be added to the provider after it's configured
		layer := Layer{
			name: layerName,
		}

		if layerConf[ConfigKeyTableName] != nil {
			tablename, err := layerConf.String(ConfigKeyTableName, &idFieldname)
			if err != nil {
				return nil, fmt.Errorf("for layer (%v) %v : %v", i, layerName, err)
			}

			layer.tablename = tablename
			layer.tagFieldnames = tagFieldnames
			layer.geomFieldname = geomTableDetails[tablename].geomFieldname
			layer.geomType = geomTableDetails[tablename].geomType
			layer.idFieldname = idFieldname
			layer.srid = geomTableDetails[tablename].srid
			layer.bbox = geomTableDetails[tablename].bbox

		} else {
			var customSQL string
			customSQL, err = layerConf.String(ConfigKeySQL, &customSQL)
			if err != nil {
				return nil, fmt.Errorf("for %v layer(%v) %v has an error: %v", i, layerName, ConfigKeySQL, err)
			}
			layer.sql = customSQL

			// if a !ZOOM! token exists, all features could be filtered out so we don't have a geometry to inspect it's type.
			// TODO(arolek): implement an SQL parser or figure out a different approach. this is brittle but I can't figure out a better
			// solution without using an SQL parser on custom SQL statements
			allZoomsSQL := "IN (0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24)"
			tokenReplacer := strings.NewReplacer(
				">= "+zoomToken, allZoomsSQL,
				">="+zoomToken, allZoomsSQL,
				"=> "+zoomToken, allZoomsSQL,
				"=>"+zoomToken, allZoomsSQL,
				"=< "+zoomToken, allZoomsSQL,
				"=<"+zoomToken, allZoomsSQL,
				"<= "+zoomToken, allZoomsSQL,
				"<="+zoomToken, allZoomsSQL,
				"!= "+zoomToken, allZoomsSQL,
				"!="+zoomToken, allZoomsSQL,
				"= "+zoomToken, allZoomsSQL,
				"="+zoomToken, allZoomsSQL,
				"> "+zoomToken, allZoomsSQL,
				">"+zoomToken, allZoomsSQL,
				"< "+zoomToken, allZoomsSQL,
				"<"+zoomToken, allZoomsSQL,
			)

			customSQL = tokenReplacer.Replace(customSQL)

			// Set bounds & zoom params to include all layers
			// Bounds checks need params: maxx, minx, maxy, miny
			// TODO(arolek): this assumes WGS84. should be more flexible
			customSQL = replaceTokens(customSQL, 0, geom.BoundingBox{{180.0, 85.0511}, {-180.0, -85.0511}})

			// Get geometry type & srid from geometry of first row.
			qtext := fmt.Sprintf("SELECT geom FROM (%v) LIMIT 1;", customSQL)

			log.Debugf("qtext: %v", qtext)

			var geomData []byte
			err = db.QueryRow(qtext).Scan(&geomData)
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("layer '%v' with custom SQL has 0 rows: %v", layerName, customSQL)
			} else if err != nil {
				return nil, fmt.Errorf("layer '%v' problem executing custom SQL: %v", layerName, err)
			}

			h, geo, err := decodeGeometry(geomData)
			if err != nil {
				return nil, err
			}

			layer.geomType = geo
			layer.srid = uint64(h.SRSId())
			layer.geomFieldname = DefaultGeomFieldName
			layer.idFieldname = DefaultIDFieldName
		}

		p.layers[layer.name] = layer
	}

	// track the provider so we can clean it up later
	providers = append(providers, p)

	return &p, err
}

// reference to all instantiated proivders
var providers []Provider

// Cleanup will close all database connections and destory all previously instantiated Provider instances
func Cleanup() {
	for i := range providers {
		if err := providers[i].Close(); err != nil {
			log.Errorf("err closing connection: %v", err)
		}
	}

	providers = make([]Provider, 0)
}
