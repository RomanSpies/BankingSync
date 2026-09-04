package actual

import (
	"regexp"
	"testing"
	"time"
)

func makeRuleDB(t *testing.T) *DB {
	t.Helper()
	return newTestDB(t)
}

func txnWith(payeeName, notes string, amountCents int64) *Transaction {
	return &Transaction{
		PayeeID:       "pid-1",
		PayeeName:     payeeName,
		ImportedPayee: payeeName,
		Notes:         notes,
		AmountCents:   amountCents,
		AccountID:     "acct-1",
	}
}

func TestNormalise(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HELLO", "hello"},
		{"café", "café"},
		{"", ""},
		{"Mixed CASE", "mixed case"},
	}
	for _, tc := range cases {
		if got := normalise(tc.in); got != tc.want {
			t.Errorf("normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMatchString(t *testing.T) {
	cases := []struct {
		field, op, pattern string
		want               bool
	}{
		{"hello", "is", "hello", true},
		{"hello", "is", "HELLO", true},
		{"hello", "is", "world", false},
		{"hello", "isNot", "world", true},
		{"hello", "isNot", "hello", false},
		{"hello world", "contains", "world", true},
		{"hello world", "contains", "missing", false},
		{"hello world", "doesNotContain", "missing", true},
		{"hello world", "doesNotContain", "world", false},
		{"hello", "oneOf", "hello", true},
		{"hello", "oneOf", "world", false},
		{"hello", "notOneOf", "world", true},
		{"hello", "notOneOf", "hello", false},
		{"hello world", "matches", "world", true},
		{"hello world", "matches", "missing", false},
		{"hello world", "matches", "^hello", true},
		{"hello world", "matches", "^world", false},
		{"hello world", "matches", `hel+o`, true},
		{"hello", "matches", "[invalid", false},
		{"hello", "unknown_op", "hello", false},
	}
	for _, tc := range cases {
		got := matchString(tc.field, tc.op, tc.pattern)
		if got != tc.want {
			t.Errorf("matchString(%q, %q, %q) = %v, want %v",
				tc.field, tc.op, tc.pattern, got, tc.want)
		}
	}
}

func TestMatchNumber(t *testing.T) {
	cases := []struct {
		cents int64
		op    string
		value any
		want  bool
	}{
		{1000, "is", float64(1000), true},
		{999, "is", float64(1000), false},
		{-3995, "is", float64(-3995), true},
		{-3995, "is", float64(3995), false},

		{1050, "isapprox", float64(1000), true},
		{1101, "isapprox", float64(1000), false},
		{-1050, "isapprox", float64(-1000), true},

		{1001, "gt", float64(1000), true},
		{1000, "gt", float64(1000), false},
		{1000, "gte", float64(1000), true},
		{999, "gte", float64(1000), false},
		{999, "lt", float64(1000), true},
		{1000, "lt", float64(1000), false},
		{1000, "lte", float64(1000), true},
		{1001, "lte", float64(1000), false},
		{1000, "unknown", float64(1000), false},
	}
	for _, tc := range cases {
		got := matchNumber(tc.cents, tc.op, tc.value)
		if got != tc.want {
			t.Errorf("matchNumber(%d, %q, %v) = %v, want %v",
				tc.cents, tc.op, tc.value, got, tc.want)
		}
	}
}

func TestMatchID_string(t *testing.T) {
	if !matchID("acct-1", "is", "acct-1") {
		t.Error("expected match for string 'is'")
	}
	if matchID("acct-1", "is", "acct-2") {
		t.Error("expected no match for different string")
	}
}

func TestMatchID_list(t *testing.T) {
	list := []any{"acct-1", "acct-2", "acct-3"}
	if !matchID("acct-2", "oneOf", list) {
		t.Error("expected match in list")
	}
	if matchID("acct-9", "oneOf", list) {
		t.Error("expected no match for missing item")
	}
}

func TestMatchBool(t *testing.T) {
	cases := []struct {
		field string
		op    string
		value any
		want  bool
	}{
		{"1", "is", true, true},
		{"true", "is", true, true},
		{"", "is", false, true},
		{"0", "is", false, true},
		{"1", "is", false, false},
		{"anything", "isNot", true, false},
	}
	for _, tc := range cases {
		got := matchBool(tc.field, tc.op, tc.value)
		if got != tc.want {
			t.Errorf("matchBool(%q, %q, %v) = %v, want %v",
				tc.field, tc.op, tc.value, got, tc.want)
		}
	}
}

func TestGetField(t *testing.T) {
	t2 := &Transaction{
		PayeeID:       "p1",
		PayeeName:     "Netflix",
		ImportedPayee: "NETFLIX INC",
		Notes:         "sub note",
		AccountID:     "a1",
	}
	cases := []struct{ field, want string }{
		{"payee", "p1"},
		{"payee_name", "Netflix"},
		{"imported_payee", "NETFLIX INC"},
		{"notes", "sub note"},
		{"account", "a1"},
		{"unknown", ""},
	}
	for _, tc := range cases {
		got := getField(t2, tc.field)
		if got != tc.want {
			t.Errorf("getField(txn, %q) = %q, want %q", tc.field, got, tc.want)
		}
	}
}

func TestRuleEvaluate_andConditions(t *testing.T) {
	r := &Rule{
		CondOp: "and",
		Conditions: []Condition{
			{Field: "payee_name", Op: "contains", Value: "netflix", Type: "string"},
			{Field: "notes", Op: "contains", Value: "sub", Type: "string"},
		},
	}
	txn := txnWith("Netflix", "subscription", -999)
	if !r.evaluate(txn) {
		t.Error("expected rule to match (and: both conditions true)")
	}

	txn2 := txnWith("Netflix", "other", -999)
	if r.evaluate(txn2) {
		t.Error("expected rule not to match (and: second condition false)")
	}
}

func TestRuleEvaluate_orConditions(t *testing.T) {
	r := &Rule{
		CondOp: "or",
		Conditions: []Condition{
			{Field: "payee_name", Op: "is", Value: "Netflix", Type: "string"},
			{Field: "payee_name", Op: "is", Value: "Spotify", Type: "string"},
		},
	}
	if !r.evaluate(txnWith("Netflix", "", -999)) {
		t.Error("expected match for Netflix (or)")
	}
	if !r.evaluate(txnWith("Spotify", "", -999)) {
		t.Error("expected match for Spotify (or)")
	}
	if r.evaluate(txnWith("Amazon", "", -999)) {
		t.Error("expected no match for Amazon (or)")
	}
}

func TestRuleEvaluate_numberCondition(t *testing.T) {
	r := &Rule{
		CondOp: "and",
		Conditions: []Condition{
			{Field: "amount", Op: "lt", Value: float64(0), Type: "number"},
		},
	}
	if !r.evaluate(txnWith("", "", -100)) {
		t.Error("expected match for negative amount")
	}
	if r.evaluate(txnWith("", "", 100)) {
		t.Error("expected no match for positive amount")
	}
}

func TestApplyAction_category(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "category", Op: "set", Value: "cat-uuid-123", Type: "id"}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}

	var cat string
	_ = d.sql.QueryRow(`SELECT COALESCE(category,'') FROM transactions WHERE id = ?`, txn.ID).Scan(&cat)
	if cat != "cat-uuid-123" {
		t.Errorf("category in DB: got %q, want cat-uuid-123", cat)
	}
	changes := d.FlushChanges()
	if len(changes) == 0 {
		t.Error("expected tracked change for category")
	}
}

func TestApplyAction_category_emptySkips(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "category", Value: ""}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	changes := d.FlushChanges()
	if len(changes) != 0 {
		t.Errorf("expected no changes for empty category, got %d", len(changes))
	}
}

func TestApplyAction_payee(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	insertPayee(t, d, "p-known", "Canonical Name")
	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "old payee", "", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "payee", Value: "p-known", Type: "id"}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}
	if txn.PayeeID != "p-known" {
		t.Errorf("PayeeID: got %q, want p-known", txn.PayeeID)
	}
	if txn.PayeeName != "Canonical Name" {
		t.Errorf("PayeeName: got %q, want Canonical Name", txn.PayeeName)
	}
}

func TestApplyAction_notes_set(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "original", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "notes", Op: "set-notes", Value: "replaced"}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}
	if txn.Notes != "replaced" {
		t.Errorf("Notes: got %q, want replaced", txn.Notes)
	}
}

func TestApplyAction_notes_prepend(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "original", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "notes", Op: "prepend-notes", Value: "PREFIX"}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}
	if txn.Notes != "PREFIXoriginal" {
		t.Errorf("Notes: got %q, want %q", txn.Notes, "PREFIXoriginal")
	}
}

func TestApplyAction_notes_append(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "original", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "notes", Op: "append-notes", Value: "SUFFIX"}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}
	if txn.Notes != "originalSUFFIX" {
		t.Errorf("Notes: got %q, want %q", txn.Notes, "originalSUFFIX")
	}
}

func TestApplyAction_cleared(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Shop", "", -100, false, "", "")
	d.FlushChanges()

	action := Action{Field: "cleared", Value: true}
	if err := applyAction(d, txn, action); err != nil {
		t.Fatalf("applyAction error: %v", err)
	}
	if !txn.Cleared {
		t.Error("expected Cleared = true after action")
	}
	var cleared int
	_ = d.sql.QueryRow(`SELECT cleared FROM transactions WHERE id = ?`, txn.ID).Scan(&cleared)
	if cleared != 1 {
		t.Errorf("DB cleared: got %d, want 1", cleared)
	}
}

func TestLoadRules_empty(t *testing.T) {
	d := makeRuleDB(t)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rs.Rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(rs.Rules))
	}
}

func TestLoadRules_parsesRules(t *testing.T) {
	d := makeRuleDB(t)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES (
			'r1', 'pre', 'and',
			'[{"field":"payee_name","op":"contains","value":"netflix","type":"string"}]',
			'[{"field":"category","op":"set","value":"cat-streaming","type":"id"}]',
			0
		)
	`)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rs.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rs.Rules))
	}
	r := rs.Rules[0]
	if len(r.Conditions) != 1 || r.Conditions[0].Field != "payee_name" {
		t.Errorf("unexpected conditions: %+v", r.Conditions)
	}
	if len(r.Actions) != 1 || r.Actions[0].Field != "category" {
		t.Errorf("unexpected actions: %+v", r.Actions)
	}
}

func TestLoadRules_skipsTombstoned(t *testing.T) {
	d := makeRuleDB(t)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES ('r-dead', 'pre', 'and', '[]', '[]', 1)
	`)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rs.Rules) != 0 {
		t.Errorf("expected tombstoned rule to be skipped, got %d rules", len(rs.Rules))
	}
}

func TestRuleSet_Match(t *testing.T) {
	rs := &RuleSet{
		Rules: []*Rule{
			{
				CondOp: "and",
				Conditions: []Condition{
					{Field: "payee_name", Op: "contains", Value: "netflix", Type: "string"},
				},
				Actions: []Action{{Field: "category", Value: "cat-1"}},
			},
			{
				CondOp: "and",
				Conditions: []Condition{
					{Field: "payee_name", Op: "is", Value: "spotify", Type: "string"},
				},
				Actions: []Action{{Field: "category", Value: "cat-2"}},
			},
		},
	}

	matched := rs.Match(txnWith("Netflix", "", -999))
	if len(matched) != 1 {
		t.Errorf("expected 1 match, got %d", len(matched))
	}

	noMatch := rs.Match(txnWith("Amazon", "", -999))
	if len(noMatch) != 0 {
		t.Errorf("expected 0 matches for Amazon, got %d", len(noMatch))
	}
}

func TestRule_Apply_endToEnd(t *testing.T) {
	d := makeRuleDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(today(), acct, "Netflix", "", -1500, false, "", "")
	d.FlushChanges()

	r := &Rule{
		Actions: []Action{
			{Field: "notes", Op: "set-notes", Value: "Streaming"},
			{Field: "cleared", Value: true},
		},
	}
	r.Apply(d, txn)

	if txn.Notes != "Streaming" {
		t.Errorf("Notes: got %q, want Streaming", txn.Notes)
	}
	if !txn.Cleared {
		t.Error("expected Cleared = true after rule apply")
	}
}

func today() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

func TestMatchCondition_stringOneOf_list(t *testing.T) {
	txn := &Transaction{PayeeName: "Netflix"}

	c := Condition{Field: "payee_name", Op: "oneOf", Value: []any{"Netflix", "Spotify"}, Type: "string"}
	if !matchCondition(txn, c) {
		t.Error("string oneOf list: expected match for Netflix")
	}

	c = Condition{Field: "payee_name", Op: "oneOf", Value: []any{"Amazon", "Spotify"}, Type: "string"}
	if matchCondition(txn, c) {
		t.Error("string oneOf list: expected no match for Netflix not in list")
	}

	c = Condition{Field: "payee_name", Op: "notOneOf", Value: []any{"Amazon", "Spotify"}, Type: "string"}
	if !matchCondition(txn, c) {
		t.Error("string notOneOf list: expected match when Netflix not in exclusion list")
	}

	c = Condition{Field: "payee_name", Op: "notOneOf", Value: []any{"Netflix", "Spotify"}, Type: "string"}
	if matchCondition(txn, c) {
		t.Error("string notOneOf list: expected no match when Netflix is in exclusion list")
	}
}

func TestMatchCondition_matchesRegex(t *testing.T) {
	txn := &Transaction{PayeeName: "Netflix Inc"}

	c := Condition{Field: "payee_name", Op: "matches", Value: "^netflix", Type: "string"}
	if !matchCondition(txn, c) {
		t.Error("matches: expected anchored regex to match")
	}

	c = Condition{
		Field:         "payee_name",
		Op:            "matches",
		Value:         `netfl.* inc`,
		Type:          "string",
		compiledRegex: regexp.MustCompile(`netfl.* inc`),
	}
	if !matchCondition(txn, c) {
		t.Error("matches: expected precompiled regex to match")
	}

	c = Condition{Field: "payee_name", Op: "matches", Value: "[invalid", Type: "string"}
	if matchCondition(txn, c) {
		t.Error("matches: expected invalid regex to return false")
	}
}

func TestMatchNumber_isbetween(t *testing.T) {
	between := func(num1, num2 float64) map[string]any {
		return map[string]any{"num1": num1, "num2": num2}
	}
	cases := []struct {
		cents int64
		value any
		want  bool
	}{
		{-3000, between(-5000, -1000), true},
		{-5000, between(-5000, -1000), true},
		{-1000, between(-5000, -1000), true},
		{-6000, between(-5000, -1000), false},
		{0, between(-5000, -1000), false},
		{-3000, between(-1000, -5000), true},
		{-3000, "not a map", false},
	}
	for _, tc := range cases {
		got := matchNumber(tc.cents, "isbetween", tc.value)
		if got != tc.want {
			t.Errorf("matchNumber(%d, isbetween, %v) = %v, want %v", tc.cents, tc.value, got, tc.want)
		}
	}
}

func TestMatchBool_isNot(t *testing.T) {
	cases := []struct {
		field string
		value any
		want  bool
	}{
		{"1", false, true},
		{"1", true, false},
		{"", false, false},
		{"", true, true},
	}
	for _, tc := range cases {
		got := matchBool(tc.field, "isNot", tc.value)
		if got != tc.want {
			t.Errorf("matchBool(%q, isNot, %v) = %v, want %v", tc.field, tc.value, got, tc.want)
		}
	}
}

func TestLoadRules_compilesMatchesRegex(t *testing.T) {
	d := makeRuleDB(t)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES (
			'r-matches', 'pre', 'and',
			'[{"field":"payee_name","op":"matches","value":"^netflix","type":"string"}]',
			'[]', 0
		)
	`)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rs.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rs.Rules))
	}
	c := rs.Rules[0].Conditions[0]
	if c.compiledRegex == nil {
		t.Error("expected compiledRegex to be set for 'matches' condition")
	}
	txn := &Transaction{PayeeName: "Netflix Inc"}
	if !matchCondition(txn, c) {
		t.Error("expected rule to match Netflix Inc via compiled regex")
	}
}

func TestLoadRules_invalidMatchesRegexWarns(t *testing.T) {
	d := makeRuleDB(t)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES (
			'r-bad-regex', 'pre', 'and',
			'[{"field":"payee_name","op":"matches","value":"[invalid","type":"string"}]',
			'[]', 0
		)
	`)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rs.Rules) != 1 {
		t.Fatalf("expected rule to be loaded despite bad regex, got %d rules", len(rs.Rules))
	}
	c := rs.Rules[0].Conditions[0]
	if c.compiledRegex != nil {
		t.Error("expected nil compiledRegex for invalid pattern")
	}
	// must not panic, must return false
	txn := &Transaction{PayeeName: "Netflix"}
	if matchCondition(txn, c) {
		t.Error("expected false for condition with invalid regex")
	}
}

func TestToString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{float64(3.14), "3.14"},
		{float64(42), "42"},
		{true, "true"},
		{false, "false"},
	}
	for _, tc := range cases {
		got := toString(tc.in)
		if got != tc.want {
			t.Errorf("toString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5},
		{int(3), 3.0},
		{"2.5", 2.5},
		{"0", 0.0},
		{"bad", 0.0},
		{nil, 0.0},
		{true, 0.0},
	}
	for _, tc := range cases {
		got := toFloat(tc.in)
		if got != tc.want {
			t.Errorf("toFloat(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestToBool(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{true, true},
		{false, false},
		{float64(1), true},
		{float64(0), false},
		{"yes", true},
		{"", false},
		{"0", false},
		{"false", false},
		{nil, false},
	}
	for _, tc := range cases {
		got := toBool(tc.in)
		if got != tc.want {
			t.Errorf("toBool(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMatchCondition_id(t *testing.T) {
	txn := &Transaction{PayeeID: "p-1", AmountCents: -1000}

	// id / string match
	c := Condition{Field: "payee", Op: "is", Value: "p-1", Type: "id"}
	if !matchCondition(txn, c) {
		t.Error("id/string: expected match")
	}

	// id / list match
	c = Condition{Field: "payee", Op: "oneOf", Value: []any{"p-1", "p-2"}, Type: "id"}
	if !matchCondition(txn, c) {
		t.Error("id/list: expected match")
	}

	// id / list no match
	c = Condition{Field: "payee", Op: "oneOf", Value: []any{"p-9"}, Type: "id"}
	if matchCondition(txn, c) {
		t.Error("id/list: expected no match")
	}
}

func TestMatchCondition_boolean(t *testing.T) {
	txn := &Transaction{Cleared: false}

	c := Condition{Field: "cleared", Op: "is", Value: false, Type: "boolean"}
	if !matchCondition(txn, c) {
		t.Error("boolean: expected match for cleared=false")
	}

	c = Condition{Field: "cleared", Op: "is", Value: true, Type: "boolean"}
	if matchCondition(txn, c) {
		t.Error("boolean: expected no match for cleared=false when checking true")
	}
}

func TestMatchCondition_importedPayee(t *testing.T) {
	txn := &Transaction{ImportedPayee: "NETFLIX INC"}

	c := Condition{Field: "imported_payee", Op: "contains", Value: "netflix", Type: "imported_payee"}
	if !matchCondition(txn, c) {
		t.Error("imported_payee type: expected match")
	}
}

func TestMatchCondition_unknownType(t *testing.T) {
	txn := &Transaction{PayeeName: "Netflix"}

	c := Condition{Field: "payee_name", Op: "is", Value: "Netflix", Type: "unknown_type"}
	if matchCondition(txn, c) {
		t.Error("unknown type: expected no match")
	}
}

func TestLoadRules_badConditionsJSON(t *testing.T) {
	d := makeRuleDB(t)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES ('r-bad-cond', 'pre', 'and', 'NOT JSON', '[]', 0)
	`)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES ('r-bad-act', 'pre', 'and', '[]', 'NOT JSON', 0)
	`)
	_, _ = d.sql.Exec(`
		INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		VALUES ('r-valid', 'pre', '', '[]', '[]', 0)
	`)
	rs, err := d.LoadRules()
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rs.Rules) != 1 {
		t.Errorf("expected 1 valid rule (bad JSON skipped), got %d", len(rs.Rules))
	}
	// empty condOp defaults to "and"
	if rs.Rules[0].CondOp != "and" {
		t.Errorf("condOp: got %q, want 'and'", rs.Rules[0].CondOp)
	}
}
