/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package migrate

const (
	unknownType dataType = iota
	intType
	stringType
	floatType
	doubleType
	datetimeType
	uidType // foreign key reference, which would correspond to uid type in Dgraph
)

// the typeToString map is used to generate the Dgraph schema file
var typeToString map[dataType]string

// the sqlTypeToInternal map is used to parse date types in SQL schema
var sqlTypeToInternal map[string]dataType

func initDataTypes() {
	typeToString = make(map[dataType]string)
	typeToString[unknownType] = "unknown"
	typeToString[intType] = "int"
	typeToString[stringType] = "string"
	typeToString[floatType] = "float"
	typeToString[doubleType] = "double"
	typeToString[datetimeType] = "datetime"
	typeToString[uidType] = "uid"

	sqlTypeToInternal = make(map[string]dataType)
	sqlTypeToInternal["int"] = intType
	sqlTypeToInternal["tinyint"] = intType
	sqlTypeToInternal["varchar"] = stringType
	sqlTypeToInternal["text"] = stringType
	sqlTypeToInternal["date"] = datetimeType
	sqlTypeToInternal["time"] = datetimeType
	sqlTypeToInternal["datetime"] = datetimeType
	sqlTypeToInternal["float"] = floatType
	sqlTypeToInternal["double"] = doubleType
	sqlTypeToInternal["decimal"] = floatType
}

func (t dataType) String() string {
	return typeToString[t]
}
