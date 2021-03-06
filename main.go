package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"os"

	"github.com/kovetskiy/godocs"
	"github.com/percona/go-mysql/query"
	hierr "github.com/reconquest/hierr-go"
	"github.com/reconquest/ser-go"
	"github.com/xwb1989/sqlparser"
)

var (
	version = "[manual build]"
	usage   = "poke " + version + `

poke is summoned for analysing MySQL slow query logs, poke reads time and
query_time fields and adds additional field time_start (time - query_time);

poke outputs records in JSON format only.

Usage:
    poke [options]
    poke -h | --help
    poke --version

Options:
    -f --file <path>  Specify file location to read.
    -h --help         Show this screen.
    --version         Show version.
`
)

var (
	rules = map[string]string{
		"Time":                  `datetime`,
		"Schema":                `string`,
		"Query_time":            `time`,
		"Lock_time":             `time`,
		"Rows_sent":             `int`,
		"Rows_examined":         `int`,
		"Rows_affected":         `int`,
		"Rows_read":             `int`,
		"Bytes_sent":            `int`,
		"Tmp_tables":            `int`,
		"Tmp_disk_tables":       `int`,
		"Tmp_table_sizes":       `int`,
		"QC_Hit":                `bool`,
		"Full_scan":             `bool`,
		"Full_join":             `bool`,
		"Tmp_table":             `bool`,
		"Tmp_table_on_disk":     `bool`,
		"Filesort":              `bool`,
		"Filesort_on_disk":      `bool`,
		"Merge_passes":          `int`,
		"InnoDB_IO_r_ops":       `int`,
		"InnoDB_IO_r_bytes":     `int`,
		"InnoDB_IO_r_wait":      `time`,
		"InnoDB_rec_lock_wait":  `time`,
		"InnoDB_queue_wait":     `time`,
		"InnoDB_pages_distinct": `int`,
	}

	regexps = map[string]*regexp.Regexp{}
)

type Record map[string]interface{}

func compileRegexps() {
	for key, rule := range rules {
		var data string

		switch rule {
		case "datetime":
			data = `.*`
		case "string":
			data = `\w+`
		case "time":
			data = `[0-9\.]+`
		case "int":
			data = `\d+`
		case "bool":
			data = `\w+`
		default:
			panic("uknown rule: " + rule)
		}

		regexps[key] = regexp.MustCompile(
			`^# .*` + key + `: (` + data + `)`,
		)
	}
}

func main() {
	args := godocs.MustParse(usage, version, godocs.UsePager)

	compileRegexps()
	inputReader := os.Stdin
	filename, ok := args["--file"].(string)
	if ok && filename != "" {
		file, err := os.Open(filename)
		if err != nil {
			hierr.Fatalf(
				err, "can't open file: %s", filename,
			)
		}
		inputReader = file
	}
	var (
		reader = bufio.NewReader(inputReader)
		record = Record{}
		// records = []Record{}
	)

	var line string
	for {
		data, isPrefix, err := reader.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}

			hierr.Fatalf(
				err, "can't read input data",
			)
		}

		if isPrefix {
			line += string(data)
			continue
		}

		line = string(data)

		if strings.HasPrefix(line, "# Time: ") {
			if len(record) > 0 {
				if record, ok := process(record); ok {
					record = prepare(record)
					// records = append(records, record)
				}
			}

			record = Record{}
		}

		if !strings.HasPrefix(line, "#") {
			if "" == getQueryType(line) {
				continue
			}
		}

		err = unmarshal(line, record)
		if err != nil {
			hierr.Fatalf(err, "unmarshal error")
		}
	}

	if record, ok := process(record); ok {
		record = prepare(record)
		// records = append(records, record)
	}

	// data, err := json.MarshalIndent(records, "", "  ")
	// if err != nil {
	// 	hierr.Fatalf(
	// 		err, "unable to encode records to JSON",
	// 	)
	// }
	// fmt.Println(string(data))
}

func process(record Record) (Record, bool) {
	if timeEnd, ok := record["time"].(time.Time); ok {
		if queryTime, ok := record["query_time"].(time.Duration); ok {
			record["time_start"] = timeEnd.Add(queryTime * -1)
		}
	} else {
		return record, false
	}

	if rawquery, ok := record["query"].(string); ok {
		record["query_length"] = len(rawquery)

		record["query_type"] = getQueryType(rawquery)

		newQuery := query.Fingerprint(rawquery)
		fingerprint := query.Id(newQuery)
		record["query_digest"] = newQuery
		record["fingerprintID"] = fingerprint

		stmt, err := sqlparser.Parse(rawquery)
		if err != nil {
			// Do something with the err
			log.Println(err)
			return record, false
		}

		// Otherwise do something with stmt

		var tableName string
		nstring := sqlparser.String
		switch s := stmt.(type) {
		case *sqlparser.Select:
			tableName = GetTablePtrsName(s.From)
		case *sqlparser.Insert:
			tableName = nstring(s.Table)
		case *sqlparser.Update:
			tableName = GetTablePtrsName(s.TableExprs)
		case *sqlparser.Delete:
			tableName = GetTablePtrsName(s.TableExprs)
		default:
			log.Printf(`unsupport prepare sql "%s", %v\n`, rawquery, s)
			return record, false
		}
		record["table"] = tableName
	}

	return record, true
}

func prepare(record Record) Record {
	for key, value := range record {
		switch value := value.(type) {
		case time.Time:
			record[key] = value.Format("2006-01-02T15:04:05.000000Z")

		case time.Duration:
			record[key] = value.Seconds()
		}
	}

	if output, err := json.Marshal(record); err != nil {
		hierr.Fatalf(err, "Ouput Marshal error %v", record)
	} else {
		fmt.Println(string(output))
	}
	return record
}

func unmarshal(line string, record Record) error {
	if !strings.HasPrefix(line, "# ") {

		_, ok := record["query"]
		if ok {
			record["query"] = record["query"].(string) + line
			return nil
		}

		record["query"] = line
	}

	for key, rule := range rules {
		raw, ok := match(line, key)
		if !ok {
			continue
		}

		value, err := parse(raw, key, rule)
		if err != nil {
			return ser.Errorf(
				err, "unable to parse %s: %s",
				key, raw,
			)
		}

		record[strings.ToLower(key)] = value
	}

	return nil
}

func match(data, key string) (string, bool) {
	matches := regexps[key].FindStringSubmatch(data)
	if len(matches) > 0 {
		return matches[1], true
	}

	return "", false
}

func parse(raw, key, rule string) (interface{}, error) {
	switch rule {
	case "datetime":
		return time.Parse("2006-01-02T15:04:05.000000Z", raw)
	case "time":
		return time.ParseDuration(raw + "s")
	case "string":
		return raw, nil
	case "int":
		return strconv.ParseInt(raw, 10, 64)
	case "bool":
		switch raw {
		case "Yes":
			return true, nil
		case "No":
			return false, nil
		default:
			return false, errors.New("invalid syntax: expected Yes or No")
		}
	}

	return nil, nil
}

func getQueryType(query string) string {
	operations := []string{
		"SELECT",
		"INSERT",
		"UPDATE",
		"DELETE",
		"DROP",
		"REPLACE",
	}

	min := -1
	queryType := ""
	for _, operation := range operations {
		index := strings.Index(query, operation)
		if index >= 0 && (index <= min || min == -1) {
			queryType = operation
			min = index
		}
	}

	if min >= 0 {
		return queryType
	}

	return ""
}
func WalkTableName(node sqlparser.SQLNode, nameList map[string]interface{}, indent int) (kontinue bool, err error) {
	if indent >= 0 {
		indent++
		for i := 0; i < indent; i++ {
			fmt.Print("-")
		}
		fmt.Printf("%T\n", node)
	}

	switch tn := node.(type) {
	case *sqlparser.AliasedTableExpr:
	case sqlparser.TableExprs:
		tn.WalkSubtree(func(node sqlparser.SQLNode) (kontinue bool, err error) {
			return WalkTableName(node, nameList, indent)
		})
		return false, nil
	case *sqlparser.AliasedExpr:
		return false, nil
	case *sqlparser.ComparisonExpr:
		return false, nil
	case *sqlparser.Where:
		return false, nil
	case sqlparser.TableName:
		if indent >= 0 {
			for i := 0; i < indent; i++ {
				fmt.Print("-")
			}
			fmt.Print("-")
			fmt.Println(sqlparser.String(tn))
		}
		name := sqlparser.String(tn)
		if len(name) > 0 {
			nameList[name] = name
		}
		return false, nil
	}

	return true, nil
}

func GetTablePtrsName(te sqlparser.TableExprs) string {
	namelist := make(map[string]interface{})
	te.WalkSubtree(func(node sqlparser.SQLNode) (kontinue bool, err error) {
		return WalkTableName(node, namelist, -1)
	})

	keys := make([]string, 0, len(namelist))
	for k := range namelist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// fmt.Println(len(namelist))
	return strings.Join(keys, ",")
}
