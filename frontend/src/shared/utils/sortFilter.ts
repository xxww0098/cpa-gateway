/**
 * Generic sort/filter utilities for table components.
 *
 * Used by admin-users, admin-subscriptions, usage logs, and other table views
 * to perform memoized sort and filter operations on arrays of items.
 */

/** Sort direction */
export type SortDirection = 'asc' | 'desc'

/** Sort criteria definition */
export interface SortCriteria<T> {
  key: keyof T
  direction: SortDirection
}

/**
 * Comparator function type for custom field comparison.
 * Returns negative if a < b, positive if a > b, 0 if equal.
 */
export type CompareFn<T> = (a: T[keyof T], b: T[keyof T]) => number

/**
 * Default comparator that handles string, number, boolean, null, and undefined values.
 * Null/undefined values are sorted to the end regardless of direction.
 */
export function defaultCompare(a: unknown, b: unknown): number {
  // Null/undefined always sort to end
  if (a == null && b == null) return 0
  if (a == null) return 1
  if (b == null) return -1

  // String comparison (case-insensitive)
  if (typeof a === 'string' && typeof b === 'string') {
    return a.localeCompare(b)
  }

  // Number comparison
  if (typeof a === 'number' && typeof b === 'number') {
    return a - b
  }

  // Boolean comparison (true > false)
  if (typeof a === 'boolean' && typeof b === 'boolean') {
    return Number(a) - Number(b)
  }

  // Fallback: convert to string
  return String(a).localeCompare(String(b))
}

/**
 * Sorts an array of items by the given criteria.
 * Returns a new array (does not mutate the input).
 *
 * Guarantees:
 * - Output length equals input length (no items lost or duplicated)
 * - Every adjacent pair satisfies the sort comparator ordering
 * - Stable sort (preserves relative order of equal elements)
 */
export function sortItems<T>(
  items: readonly T[],
  criteria: SortCriteria<T>,
  compareFn: CompareFn<T> = defaultCompare as CompareFn<T>,
): T[] {
  const { key, direction } = criteria
  const dirMultiplier = direction === 'asc' ? 1 : -1

  return [...items].sort((a, b) => {
    const aVal = a[key]
    const bVal = b[key]
    return dirMultiplier * compareFn(aVal, bVal)
  })
}

/**
 * Filters an array of items by a predicate function.
 * Returns a new array (does not mutate the input).
 *
 * Guarantees:
 * - Output length is <= input length
 * - All output items exist in the input
 */
export function filterItems<T>(
  items: readonly T[],
  predicate: (item: T) => boolean,
): T[] {
  return items.filter(predicate)
}

/**
 * Applies filter then sort to an array of items.
 * This is the combined operation used in memoized table computations.
 *
 * Guarantees:
 * - Output length equals the number of items passing the filter
 * - Sorted output satisfies ordering for all adjacent pairs
 */
export function filterAndSort<T>(
  items: readonly T[],
  predicate: ((item: T) => boolean) | null,
  criteria: SortCriteria<T> | null,
  compareFn: CompareFn<T> = defaultCompare as CompareFn<T>,
): T[] {
  let result = predicate ? filterItems(items, predicate) : [...items]
  if (criteria) {
    result = sortItems(result, criteria, compareFn)
  }
  return result
}
