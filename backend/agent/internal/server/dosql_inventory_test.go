package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const databaseInventorySample = `[
 {"kind":"StatefulSet","metadata":{"name":"orders-postgres","namespace":"qa","uid":"uid-orders"},"spec":{"serviceName":"orders","template":{"spec":{"containers":[{"image":"postgres:16"}]}}}},
 {"kind":"Service","metadata":{"name":"orders","namespace":"qa"},"spec":{"selector":{"app":"orders"},"ports":[{"name":"postgres","port":15432}]}},
 {"kind":"Pod","metadata":{"name":"orders-postgres-0","namespace":"qa","labels":{"app":"orders"},"ownerReferences":[{"kind":"StatefulSet","name":"orders-postgres","controller":true,"uid":"uid-orders"}]},"spec":{"containers":[{"image":"postgres:16"}]}}
 ]`

func TestDatabaseInventoryGroupsOnlyVerifiedStatefulSetMembers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func([]map[string]interface{})
		wantRows     int
		wantEndpoint bool
	}{
		{"owner UID and exclusive service selector", nil, 1, true},
		{"stale owner UID keeps independent pod", func(objs []map[string]interface{}) {
			refs := objs[2]["metadata"].(map[string]interface{})["ownerReferences"].([]interface{})
			refs[0].(map[string]interface{})["uid"] = "old-uid"
		}, 2, false},
		{"selector points elsewhere", func(objs []map[string]interface{}) {
			objs[1]["spec"].(map[string]interface{})["selector"] = map[string]interface{}{"app": "other"}
		}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objects []map[string]interface{}
			if err := json.Unmarshal([]byte(databaseInventorySample), &objects); err != nil {
				t.Fatal(err)
			}
			if tc.change != nil {
				tc.change(objects)
			}
			rows := databaseInventoryRows(objects, "local", "desktop")
			if len(rows) != tc.wantRows {
				t.Fatalf("unexpected resources: %#v", rows)
			}
			for _, row := range rows {
				if row.Source == "StatefulSet" && tc.wantEndpoint {
					if row.Host != "orders.qa.svc" || row.Port != 15432 || len(row.RelatedAssetIDs) != 1 || row.RelatedAssetIDs[0] != databaseAssetID("local", "desktop", "qa", "orders-postgres-0", "postgres") {
						t.Fatalf("incomplete instance identity: %#v", row)
					}
				} else if row.Host != "" || row.Port != 0 {
					t.Fatalf("unverified address: %#v", row)
				}
			}
		})
	}
}

func TestDatabaseDiscoveryRecognizesSupportedImagesAndJDBCNames(t *testing.T) {
	for _, tc := range []struct{ image, jdbc, engine, database string }{
		{"postgres:16", "jdbc:postgresql://orders:5432/sales", "postgres", "sales"},
		{"mysql:8", "jdbc:mysql://orders:3306/sales?useSSL=true", "mysql", "sales"},
		{"gvenzl/oracle-free:23", "", "oracle", ""},
		{"container-registry.oracle.com/database/enterprise:19", "", "oracle", ""},
		{"oraclelinux:9", "", "", ""},
	} {
		t.Run(tc.image, func(t *testing.T) {
			container := map[string]interface{}{"image": tc.image, "env": []interface{}{map[string]interface{}{"name": "SPRING_DATASOURCE_URL", "value": tc.jdbc}}}
			obj := map[string]interface{}{"kind": "Pod", "metadata": map[string]interface{}{"name": "orders", "namespace": "qa"}, "spec": map[string]interface{}{"containers": []interface{}{container}}}
			rows := databaseInventoryRows([]map[string]interface{}{obj}, "local", "desktop")
			if tc.engine == "" && len(rows) == 0 {
				return
			}
			if len(rows) != 1 || rows[0].Engine != tc.engine || rows[0].DatabaseName != tc.database {
				t.Fatalf("unexpected discovery: %#v", rows)
			}
		})
	}
}

func TestDatabaseInventoryKeepsLegacyPodAssetUsable(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "kubectl"), "#!/bin/sh\ncat <<'JSON'\n{\"items\":"+databaseInventorySample+"}\nJSON\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_GATEWAY_CLUSTER", "local")
	t.Setenv("DOOPS_GATEWAY_INSTANCE", "desktop")
	id := databaseAssetID("local", "desktop", "qa", "orders-postgres-0", "postgres")
	candidate, err := authorizeDoSQLDatabaseAsset(t.Context(), doSQLParams{Engine: "postgres", DatabaseAssetID: id}, "qa")
	if err != nil || candidate.DatabaseAssetID != id || candidate.Source != "Pod" {
		t.Fatalf("legacy asset = %#v, err = %v", candidate, err)
	}
}
