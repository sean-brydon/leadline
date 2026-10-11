package store

/*
#include <malloc.h>

static void leadline_trim(void) {
#ifdef __GLIBC__
	malloc_trim(0);
#endif
}
*/
import "C"

// trimAllocator asks the system's allocator to give back the pages it is
// holding on to. DuckDB frees a query's working memory when the query ends,
// but the allocator keeps what was freed for the next one. Only glibc has
// a way to ask.
func trimAllocator() { C.leadline_trim() }
