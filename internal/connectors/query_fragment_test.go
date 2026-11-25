package connectors

import "testing"

func TestReadQueryFragmentCannotEscapeProtectedAssertion(t *testing.T) {
	for _, sql := range []string{
		"SELECT n FROM public.events) SELECT true --",
		"SELECT n FROM public.events; SELECT true",
		"SELECT 1 AS n",
		"SELECT 'from' AS n",
		"SELECT n INTO replacement FROM public.events",
		"DELETE FROM public.events",
		"SELECT n FROM public.events /*",
	} {
		if _, err := ReadQueryFragment(sql); err == nil {
			t.Fatal("unsafe fragment accepted", sql)
		}
	}
	for _, sql := range []string{"SELECT count(*) AS n FROM public.events;", "WITH c AS (SELECT n FROM public.events) SELECT n FROM c", "SELECT '(' AS literal FROM public.events -- )\n"} {
		if _, err := ReadQueryFragment(sql); err != nil {
			t.Fatal(sql, err)
		}
	}
}
