// Copyright 2020 The SQLFlow Authors. All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package couler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"sqlflow.org/sqlflow/pkg/ir"
	pb "sqlflow.org/sqlflow/pkg/proto"
)

var defaultDockerImage = "sqlflow/sqlflow"
var workflowTTL = 24 * 3600

func fillMapIfValueNotEmpty(m map[string]string, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func newSessionFromProto(session *pb.Session) map[string]string {
	envs := make(map[string]string)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_USER_TOKEN", session.Token)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_DATASOURCE", session.DbConnStr)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_USER_ID", session.UserId)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_HIVE_LOCATION", session.HiveLocation)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_HDFS_NAMENODE_ADDR", session.HdfsNamenodeAddr)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_HADOOP_USER", session.HdfsUser)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_HADOOP_PASS", session.HdfsUser)
	fillMapIfValueNotEmpty(envs, "SQLFLOW_submitter", session.Submitter)
	return envs
}

func getStepEnvs(session *pb.Session) (map[string]string, error) {
	envs := newSessionFromProto(session)
	for _, env := range os.Environ() {
		pair := strings.SplitN(env, "=", 2)
		if len(pair) != 2 {
			return nil, fmt.Errorf("env: %s should format key=value", env)
		}
		if strings.HasPrefix(pair[0], "SQLFLOW_OSS_") {
			envs[pair[0]] = pair[1]
		}
	}
	if _, ok := envs["SQLFLOW_submitter"]; !ok {
		envs["SQLFLOW_submitter"] = os.Getenv("SQLFLOW_submitter")
	}
	envs["SQLFLOW_PARSER_SERVER_PORT"] = os.Getenv("SQLFLOW_PARSER_SERVER_PORT")
	return envs, nil
}

// GenCode generates Couler program
func GenCode(programIR []ir.SQLFlowStmt, session *pb.Session) (string, error) {
	stepEnvs, err := getStepEnvs(session)
	if err != nil {
		return "", err
	}
	if os.Getenv("SQLFLOW_WORKFLOW_TTL") != "" {
		workflowTTL, err = strconv.Atoi(os.Getenv("SQLFLOW_WORKFLOW_TTL"))
		if err != nil {
			return "", fmt.Errorf("SQLFLOW_WORKFLOW_TTL: %s should be int", os.Getenv("SQLFLOW_WORKFLOW_TTL"))
		}
	}
	r := &coulerFiller{
		DataSource:  session.DbConnStr,
		StepEnvs:    stepEnvs,
		WorkflowTTL: workflowTTL,
	}
	// NOTE(yancey1989): does not use ModelImage here since the Predict statement
	// does not contain the ModelImage field in SQL Program IR.
	if os.Getenv("SQLFLOW_WORKFLOW_STEP_IMAGE") != "" {
		defaultDockerImage = os.Getenv("SQLFLOW_WORKFLOW_STEP_IMAGE")
	}

	for _, sqlIR := range programIR {
		switch i := sqlIR.(type) {
		case *ir.NormalStmt, *ir.PredictStmt, *ir.ExplainStmt:
			sqlStmt := &sqlStatement{
				OriginalSQL: sqlIR.GetOriginalSQL(), IsExtendedSQL: sqlIR.IsExtended(),
				DockerImage: defaultDockerImage}
			r.SQLStatements = append(r.SQLStatements, sqlStmt)
		case *ir.TrainStmt:
			if r.SQLFlowSubmitter == "katib" {
				sqlStmt, err := ParseKatibSQL(sqlIR.(*ir.TrainStmt))
				if err != nil {
					return "", fmt.Errorf("Fail to parse Katib train statement %s", sqlIR.GetOriginalSQL())
				}
				r.SQLStatements = append(r.SQLStatements, sqlStmt)
			} else {
				sqlStmt := &sqlStatement{
					OriginalSQL: sqlIR.GetOriginalSQL(), IsExtendedSQL: sqlIR.IsExtended(),
					DockerImage: defaultDockerImage}
				r.SQLStatements = append(r.SQLStatements, sqlStmt)
			}
		default:
			return "", fmt.Errorf("unrecognized IR type: %v", i)
		}
	}
	var program bytes.Buffer
	if err := coulerTemplate.Execute(&program, r); err != nil {
		return "", err
	}
	return program.String(), nil
}

// Compile Couler program into Argo YAML
func Compile(coulerProgram string) (string, error) {
	cmdline := bytes.Buffer{}
	fmt.Fprintf(&cmdline, "couler run --mode argo --workflow_name sqlflow ")
	if c := os.Getenv("SQLFLOW_COULER_CLUSTER_CONFIG"); len(c) > 0 {
		fmt.Fprintf(&cmdline, "--cluster_config %s ", c)
	}
	fmt.Fprintf(&cmdline, "--file -")

	coulerExec := strings.Split(cmdline.String(), " ")
	// execute command: `cat sqlflow.couler | couler run --mode argo --workflow_name sqlflow --file -`
	cmd := exec.Command(coulerExec[0], coulerExec[1:]...)
	cmd.Env = append(os.Environ())
	cmd.Stdin = strings.NewReader(coulerProgram)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed %s, %v", cmd, err)
	}
	return string(out), nil
}
