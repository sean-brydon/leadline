package store

/*
#include <malloc/malloc.h>
*/
import "C"

// trimAllocator asks the system's allocator to give back the pages it is
// holding on to. DuckDB frees a query's working memory when the query ends,
// but the allocator keeps what was freed for the next one.
func trimAllocator() { C.malloc_zone_pressure_relief(nil, 0) }
