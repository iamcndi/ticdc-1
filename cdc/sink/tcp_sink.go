// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sink

import (
	//"bufio"
	"context"
	"fmt"
	//"net"
	"sync/atomic"

	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"go.uber.org/zap"
)

// newDsgTestSink creates a block hole sink
func newDsgSink(ctx context.Context, opts map[string]string) *dsgSink {
	return &dsgSink{
		statistics: NewStatistics(ctx, "rowsocket", opts),
	}
}

type dsgSink struct {
	statistics      *Statistics
	checkpointTs    uint64
	accumulated     uint64
	lastAccumulated uint64
}

type RowJson struct {
	SchemaName    string
	TableName     string
	Columns       []*Column
}

//*每个字段的数据结构*
type Column struct {
	ColNo   *int32  `protobuf:"varint,1,opt,name=colNo" json:"colNo,omitempty"`
	ColType byte `protobuf:"bytes,2,opt,name=colType" json:"colType,omitempty"`
	//*字段名称(忽略大小写)，在mysql中是没有的*
	ColName *string `protobuf:"bytes,3,opt,name=colName" json:"colName,omitempty"`
	//* 字段标识 *
	ColFlags *int32 `protobuf:"varint,4,opt,name=colFlags" json:"colFlags,omitempty"`
	ColValue             interface{}  `protobuf:"bytes,6,opt,name=colValue" json:"colValue,omitempty"`

}

func (b *dsgSink) EmitRowChangedEvents(ctx context.Context, rows ...*model.RowChangedEvent) error {


	for _, row := range rows {
		log.Debug("dsgSocketSink: EmitRowChangedEvents", zap.Any("row", row))


		/*for _, column := range row.Columns {
			columnName := column.Name
			columnValue := model.ColumnValueString(column.Value)
			//fmt.Println(value)
			conn, err := net.Dial("tcp", ":2300")
			if err != nil {
				log.Fatal("",zap.Any("err", err))
			}

			fmt.Fprintf(conn, "columnName:"+columnName+":::columnValue:"+columnValue+"\n")
			res, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				log.Fatal("",zap.Any("err", err))
			}

			fmt.Println(string(res))
			//fmt.Fprintf(conn, value+"\n")

			conn.Close()


		}*/


	}

	///////////////////////////////////////////////////////////////////////////////
	fmt.Println(">>>>>>>>>>>>>>>>>>>>======================================================================================>>>>>>>>>>>>>>>>>>>")
	var eventTypeValue int32
	var schemaName string
	var tableName string

	if len(rows) == 0 {
		return nil
	} else {
		log.Info("PreColumns: ", zap.Any("", rows[0].PreColumns))
		log.Info("Columns: ", zap.Any("", rows[0].Columns))
		if len(rows[0].PreColumns) == 0 {
			//insert
			eventTypeValue = 2
		} else if len(rows[0].Columns) == 0 {
			//delete
			eventTypeValue = 4
		} else {
			//update
			eventTypeValue = 3
		}

		schemaName = rows[0].Table.Schema
		tableName = rows[0].Table.Table

	}

	for _, row := range rows {
		log.Info("show::::::::::::::::::::::::::::: row", zap.Any("row", row))

		//解析数据为json字符串
		rowdata := &RowJson{}
		if eventTypeValue == 2 {
			//insert
			rowdata = getRowData(0, row.Columns, rowdata)
		} else if eventTypeValue == 4 {
			//delete
			rowdata = getRowData(0, row.PreColumns, rowdata)
		} else if eventTypeValue == 3 {
			//update
			//after
			rowdata = getRowData(0, row.Columns, rowdata)
			//before
			rowdata = getRowData(1, row.PreColumns, rowdata)

		}


		rowdata.SchemaName = schemaName
		rowdata.TableName = tableName
		fmt.Println("show rowdata1 ：：：：：：：：：：：：：：", rowdata)

		log.Info("show rowdata ：：：：：：：：：：：：：：", zap.Reflect("rowdata", rowdata))
		//send

		sender(rowdata)

	}


	rowsCount := len(rows)
	atomic.AddUint64(&b.accumulated, uint64(rowsCount))
	b.statistics.AddRowsCount(rowsCount)
	return nil
}

func (b *dsgSink) FlushRowChangedEvents(ctx context.Context, resolvedTs uint64) (uint64, error) {
	log.Debug("dsgSocketSink: FlushRowChangedEvents", zap.Uint64("resolvedTs", resolvedTs))
	err := b.statistics.RecordBatchExecution(func() (int, error) {
		// TODO: add some random replication latency
		accumulated := atomic.LoadUint64(&b.accumulated)
		batchSize := accumulated - b.lastAccumulated
		b.lastAccumulated = accumulated
		return int(batchSize), nil
	})
	b.statistics.PrintStatus(ctx)
	atomic.StoreUint64(&b.checkpointTs, resolvedTs)
	return resolvedTs, err
}

/*func analysisRowsAndSend(b *dsgSocketSink, ctx context.Context, singleTableTxn *model.SingleTableTxn) error {

	var eventTypeValue int32

	rowsMap, err := analysisRows(singleTableTxn)
	if err != nil {
		return errors.Trace(err)
	}
	for dmlType, rows := range rowsMap {
		if dmlType == "I" {
			eventTypeValue = 2
			if rows != nil {
				err := send(b, ctx, singleTableTxn, rows, eventTypeValue)
				if err != nil {
					return errors.Trace(err)
				}
			}
		} else if dmlType == "U" {
			if rows != nil {
				eventTypeValue = 3
				err := send(b, ctx, singleTableTxn, rows, eventTypeValue)
				if err != nil {
					return errors.Trace(err)
				}
			}
		} else if dmlType == "D" {
			if rows != nil {
				eventTypeValue = 4
				err := send(b, ctx, singleTableTxn, rows, eventTypeValue)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
	return nil
}*/

func (b *dsgSink) EmitCheckpointTs(ctx context.Context, ts uint64) error {
	log.Debug("dsgSocketSink: Checkpoint Event", zap.Uint64("ts", ts))
	return nil
}

func (b *dsgSink) EmitDDLEvent(ctx context.Context, ddl *model.DDLEvent) error {
	log.Debug("dsgSocketSink: DDL Event", zap.Any("ddl", ddl))
	return nil
}

// Initialize is no-op for blackhole
func (b *dsgSink) Initialize(ctx context.Context, tableInfo []*model.SimpleTableInfo) error {
	return nil
}

func (b *dsgSink) Close(ctx context.Context) error {
	return nil
}

func (b *dsgSink) Barrier(ctx context.Context) error {
	return nil
}

func getRowData(colFlag int32, columns []*model.Column, json *RowJson) *RowJson {

	rowdata := &RowJson{}
	for _, column := range columns {

		columnBuilder := &Column{}
		columnBuilder.ColName = &column.Name
		//columnBuilder.ColValue = &column.Value
		columnBuilder.ColValue = model.ColumnValueString(column.Value)
		//columnBuilder.ColValue = model.ColumnValueString(column.Value, column.Flag)
		//columnBuilder.ColType = &column.Type
		fmt.Println("column.Value:::::",column.Value)
		fmt.Println("columnBuilder.ColValue:::::",columnBuilder.ColValue)
		columnBuilder.ColFlags = &colFlag
		//columnBuilder.ColType = &column.Type
		rowdata.Columns = append(rowdata.Columns,columnBuilder)
	}
	return rowdata
}
