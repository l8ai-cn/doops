package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	debversion "github.com/knqyf263/go-deb-version"
)

type inspectionPackage struct {
	Component  string `json:"component"`
	Advisory   string `json:"advisory"`
	Version    string `json:"version"`
	Introduced string `json:"introduced"`
	Fixed      string `json:"fixed"`
}
type inspectionEvidence struct {
	Observed   time.Time           `json:"observed_at"`
	Source     string              `json:"source"`
	SHA        string              `json:"sha256,omitempty"`
	Policy     string              `json:"policy,omitempty"`
	Bulletin   string              `json:"bulletin_sha256,omitempty"`
	Samples    *int                `json:"samples,omitempty"`
	Violations *int                `json:"violations,omitempty"`
	Detail     string              `json:"detail,omitempty"`
	Gap        string              `json:"gap,omitempty"`
	Packages   []inspectionPackage `json:"packages,omitempty"`
}

var inspectionSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var inspectionVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

func inspectCompare(a, b string) (int, bool) {
	if !inspectionVersion.MatchString(a) || !inspectionVersion.MatchString(b) {
		return 0, false
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i, s := range aa {
		n, _ := strconv.Atoi(s)
		m, _ := strconv.Atoi(bb[i])
		if n != m {
			return n - m, true
		}
	}
	return 0, true
}

var debianChars = regexp.MustCompile(`^(?:[0-9]+:)?[0-9][A-Za-z0-9.+:~-]*$`)
var versionDigits = regexp.MustCompile(`[0-9]+`)

func packageVersionCompare(component, a, b string) (int, bool) {
	if !strings.HasPrefix(component, "pkg:deb/") {
		return inspectCompare(a, b)
	}
	for _, v := range []string{a, b} {
		if len(v) > 128 || !debianChars.MatchString(v) || strings.HasSuffix(v, "-") {
			return 0, false
		}
		// The comparator represents numeric runs with int. Refuse any run it cannot represent.
		for _, digits := range versionDigits.FindAllString(v, -1) {
			if _, err := strconv.Atoi(digits); err != nil {
				return 0, false
			}
		}
	}
	left, errA := debversion.NewVersion(a)
	right, errB := debversion.NewVersion(b)
	// dpkg accepts epochs through 2^31-1, including on 64-bit hosts.
	if errA != nil || errB != nil || left.Epoch() > 1<<31-1 || right.Epoch() > 1<<31-1 {
		return 0, false
	}
	return left.Compare(right), true
}

func affectedInspection(p inspectionPackage) (bool, bool) {
	lo, ok := packageVersionCompare(p.Component, p.Version, p.Introduced)
	hi, ok2 := packageVersionCompare(p.Component, p.Version, p.Fixed)
	width, ok3 := packageVersionCompare(p.Component, p.Fixed, p.Introduced)
	return lo >= 0 && hi < 0, ok && ok2 && ok3 && width > 0
}
func inspectionVerdict(check string, e inspectionEvidence, before []inspectionPackage) (string, string, error) {
	if !inspectText(e.Source, 96) || !inventoryMetadata(e.Detail) || len(e.Detail) > 512 {
		return "", "", inspectErr(400, "invalid evidence source or detail")
	}
	if e.Gap != "" {
		if !inspectText(e.Gap, 200) || e.Samples != nil || e.Violations != nil || len(e.Packages) != 0 {
			return "", "", inspectErr(400, "invalid gap evidence")
		}
		return "insufficient", e.Gap, nil
	}
	if !inspectionSHA.MatchString(e.SHA) {
		return "", "", inspectErr(400, "source artifact SHA-256 required")
	}
	if check[0] != 'V' {
		if !inspectText(e.Policy, 160) || !inspectText(e.Detail, 512) || e.Samples == nil || e.Violations == nil || *e.Samples <= 0 || *e.Violations < 0 || *e.Violations > *e.Samples || len(e.Packages) > 0 {
			return "", "", inspectErr(400, "measurement requires rule, observations, violations and a redacted measurement")
		}
		if *e.Violations > 0 {
			return "finding", "policy_violation", nil
		}
		return "pass", "policy_satisfied", nil
	}
	if !inspectionSHA.MatchString(e.Bulletin) || len(e.Packages) == 0 {
		return "insufficient", "no_applicable_advisory_evidence", nil
	}
	if len(e.Packages) > 256 {
		return "", "", inspectErr(400, "too many package matches")
	}
	if check == "V3" && len(before) == 0 {
		return "insufficient", "no_registered_finding_baseline", nil
	}
	type identity struct{ pkg, id string }
	seen := map[identity]inspectionPackage{}
	finding, unknown := false, false
	for _, p := range e.Packages {
		if !inspectText(p.Component, 256) || !strings.HasPrefix(p.Component, "pkg:") || !inspectText(p.Advisory, 128) || !inspectText(p.Version, 128) || !inspectText(p.Fixed, 128) || !inspectText(p.Introduced, 128) {
			return "", "", inspectErr(400, "invalid component/advisory evidence")
		}
		k := identity{p.Component, p.Advisory}
		if _, dupe := seen[k]; dupe {
			return "", "", inspectErr(400, "ambiguous component/advisory match")
		}
		seen[k] = p
		hit, ok := affectedInspection(p)
		finding = finding || hit && ok
		unknown = unknown || !ok
	}
	for _, b := range before {
		cur, found := seen[identity{b.Component, b.Advisory}]
		if !found || cur.Introduced != b.Introduced || cur.Fixed != b.Fixed {
			return "insufficient", "baseline_advisory_not_comparable", nil
		}
		cmp, ok := packageVersionCompare(cur.Component, cur.Version, b.Fixed)
		if !ok {
			return "insufficient", "unverified_version_scheme", nil
		}
		if cmp < 0 {
			finding = true
		}
	}
	if unknown {
		return "insufficient", "unsupported_or_ambiguous_version", nil
	}
	if finding {
		return "finding", "affected_deployed_version", nil
	}
	return "pass", "outside_reviewed_advisory_range", nil
}
func inspectionBefore(tx *sql.Tx, item int64, check string, observed time.Time) ([]inspectionPackage, error) {
	rows, err := tx.Query(`SELECT e.evidence,b.repair_at FROM inspection_items i JOIN inspection_items b ON b.run_id=i.run_id AND b.asset_id=i.asset_id AND b.cluster=i.cluster AND b.instance=i.instance JOIN inspection_receipts e ON e.item_id=b.id WHERE i.id=? AND b.repair<>'' AND b.check_id IN ('V1','V2') AND (b.id=i.id OR ?='V3') AND e.status='finding' AND e.generation=(SELECT min(f.generation) FROM inspection_receipts f WHERE f.item_id=b.id AND f.status='finding')`, item, check)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []inspectionPackage
	for rows.Next() {
		var raw string
		var e inspectionEvidence
		var repaired int64
		if err := rows.Scan(&raw, &repaired); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		if observed.Unix() <= repaired || !observed.After(e.Observed) {
			return nil, inspectErr(409, "post-remediation observation required")
		}
		for _, p := range e.Packages {
			if hit, ok := affectedInspection(p); hit && ok {
				result = append(result, p)
			}
		}
	}
	return result, rows.Err()
}
func inspectionItemAction(tx *sql.Tx, a TokenAuth, q inspectionRequest, now time.Time) (any, AuditFinish, error) {
	var f AuditFinish
	var run, opened, deadline, repairAt int64
	var gen int
	var executor, check, status, reason, repair, block string
	err := tx.QueryRow(inventoryGrants+`SELECT r.id,p.executor,s.cluster,s.instance,s.check_id,s.generation,s.opened,s.deadline,`+inspectState+`,`+inspectReason+`,s.repair,s.repair_at,`+inspectBlock+inspectionJoins+` WHERE s.id=? AND `+inspectAllowed, a.UserID, a.TokenID, q.Item).Scan(&run, &executor, &f.Cluster, &f.Instance, &check, &gen, &opened, &deadline, &status, &reason, &repair, &repairAt, &block)
	if err == sql.ErrNoRows {
		return nil, f, inspectErr(404, "inspection item not found")
	}
	if err != nil {
		return nil, f, err
	}
	f.CommandSummary = fmt.Sprintf("run=%d item=%d generation=%d check=%s", run, q.Item, gen, check)
	if gen != q.Generation {
		return nil, f, inspectErr(409, "stale work generation")
	}
	out := map[string]any{"item_id": q.Item, "generation": gen}
	switch q.Action {
	case "repair":
		if status != "finding" || repair != "" || !inspectText(q.Memo, 240) {
			return nil, f, inspectErr(409, "an open finding and a short remediation record are required")
		}
		_, err = tx.Exec(`UPDATE inspection_items SET repair=?,repair_by=?,repair_at=? WHERE id=?`, q.Memo, a.UserID, now.Unix(), q.Item)
	case "gap":
		if status != "pending" || !inspectText(q.Memo, 200) {
			return nil, f, inspectErr(409, "pending item and gap reason required")
		}
		_, err = tx.Exec(`UPDATE inspection_items SET status='missing',reason=? WHERE id=?`, q.Memo, q.Item)
	case "makeup", "recheck":
		if block != "" {
			return nil, f, inspectErr(409, block)
		}
		if q.Action == "makeup" && (status != "missing" && status != "insufficient") || q.Action == "recheck" && (repair == "" || status != "finding" && status != "insufficient") {
			return nil, f, inspectErr(409, "item is not eligible for this attempt")
		}
		if status == "missing" {
			_, err = tx.Exec(`INSERT INTO inspection_receipts VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(item_id,generation) DO NOTHING`, q.Item, gen, a.UserID, now.Unix(), status, reason, "null", "")
			if err != nil {
				return nil, f, err
			}
		}
		_, err = tx.Exec(`UPDATE inspection_items SET generation=generation+1,opened=?,deadline=?,status='pending',reason='' WHERE id=?`, now.Unix()+1, now.Add(24*time.Hour).Unix(), q.Item)
		out["generation"] = gen + 1
	case "evidence":
		if a.UserID != executor {
			return nil, f, inspectErr(403, "bound executor identity required")
		}
		if block != "" {
			return nil, f, inspectErr(403, block)
		}
		if q.Evidence == nil {
			return nil, f, inspectErr(400, "evidence required")
		}
		e := *q.Evidence
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, f, err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		var stored, result string
		err = tx.QueryRow(`SELECT digest,status FROM inspection_receipts WHERE item_id=? AND generation=?`, q.Item, gen).Scan(&stored, &result)
		if err == nil {
			if stored != digest {
				return nil, f, inspectErr(409, "different evidence already recorded")
			}
			out["status"] = result
			return out, f, nil
		}
		if err != sql.ErrNoRows {
			return nil, f, err
		}
		if status != "pending" || now.Unix() >= deadline || e.Observed.Unix() < opened || e.Observed.Unix() < repairAt || e.Observed.After(time.Now().UTC()) {
			return nil, f, inspectErr(409, "evidence outside current collection window")
		}
		var before []inspectionPackage
		if check[0] == 'V' {
			before, err = inspectionBefore(tx, q.Item, check, e.Observed)
			if err != nil {
				return nil, f, err
			}
		}
		verdict, why, err := inspectionVerdict(check, e, before)
		if err != nil {
			return nil, f, err
		}
		_, err = tx.Exec(`INSERT INTO inspection_receipts VALUES(?,?,?,?,?,?,?,?)`, q.Item, gen, a.UserID, now.Unix(), verdict, why, string(raw), digest)
		if err != nil {
			return nil, f, err
		}
		_, err = tx.Exec(`UPDATE inspection_items SET status=?,reason=? WHERE id=?`, verdict, why, q.Item)
		out["status"] = verdict
		return out, f, err
	}
	return out, f, err
}
