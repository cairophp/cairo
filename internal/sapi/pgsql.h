#ifndef CAIRO_PGSQL_H
#define CAIRO_PGSQL_H

// A fully read result set, handed from Go to the PDO driver. Every array is
// malloc'd; vals[row * ncols + col] is NULL for SQL NULL. Values are in
// PostgreSQL's text format.
typedef struct cairo_pg_result {
	int ncols;
	int nrows;
	char **names;
	unsigned int *oids;
	char **vals;
	int *lens;
	long long affected;
} cairo_pg_result;

void cairo_pg_result_free(cairo_pg_result *r);
void cairo_register_pdo_pgsql(void);

#endif
