/**
 * Property-based tests for sort/filter invariants.
 *
 * Uses fast-check to verify that sortItems produces output with the same
 * length as input (no items lost or duplicated) and that every adjacent
 * pair satisfies the sort comparator ordering.
 *
 * **Validates: Requirements 7.4**
 */

import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import {
  sortItems,
  filterItems,
  filterAndSort,
  defaultCompare,
  type SortCriteria,
  type SortDirection,
} from './sortFilter'

// ---------------------------------------------------------------------------
// Test item type used across all properties
// ---------------------------------------------------------------------------

interface TestItem {
  id: number
  name: string
  email: string | null
  balance: number
  active: boolean
  createdAt: string | null
}

/** Arbitrary for generating test items */
const testItemArb = fc.record<TestItem>({
  id: fc.integer({ min: 0, max: 100000 }),
  name: fc.string({ minLength: 0, maxLength: 50 }),
  email: fc.option(fc.emailAddress(), { nil: null }),
  balance: fc.double({ min: -10000, max: 10000, noNaN: true }),
  active: fc.boolean(),
  createdAt: fc.option(
    fc.date({ min: new Date('2020-01-01'), max: new Date('2030-01-01') }).map(d => d.toISOString()),
    { nil: null }
  ),
})

/** Valid sort keys for TestItem */
const sortKeyArb = fc.constantFrom<keyof TestItem>('id', 'name', 'email', 'balance', 'active', 'createdAt')

/** Sort direction arbitrary */
const sortDirectionArb = fc.constantFrom<SortDirection>('asc', 'desc')

/** Sort criteria arbitrary */
const sortCriteriaArb: fc.Arbitrary<SortCriteria<TestItem>> = fc.record({
  key: sortKeyArb,
  direction: sortDirectionArb,
})

// ---------------------------------------------------------------------------
// Property 8: Sort/Filter Invariants
// Validates: Requirements 7.4
// ---------------------------------------------------------------------------

describe('Property 8: Sort/Filter Invariants', () => {
  /**
   * **Validates: Requirements 7.4**
   *
   * For any array of items and any valid sort criteria, the sorted output
   * has the same length as the input (no items lost or duplicated).
   */
  it('sorted output has the same length as input for all valid sort criteria', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 100 }),
        sortCriteriaArb,
        (items, criteria) => {
          const sorted = sortItems(items, criteria)
          expect(sorted).toHaveLength(items.length)
        }
      )
    )
  })

  /**
   * **Validates: Requirements 7.4**
   *
   * For any array of items and any valid sort criteria, every adjacent pair
   * in the sorted output satisfies the sort comparator ordering.
   */
  it('every adjacent pair satisfies the sort comparator ordering', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 100 }),
        sortCriteriaArb,
        (items, criteria) => {
          const sorted = sortItems(items, criteria)
          const { key, direction } = criteria
          const dirMultiplier = direction === 'asc' ? 1 : -1

          for (let i = 0; i < sorted.length - 1; i++) {
            const cmp = defaultCompare(sorted[i][key], sorted[i + 1][key])
            // In correct ordering, dirMultiplier * cmp should be <= 0
            expect(dirMultiplier * cmp).toBeLessThanOrEqual(0)
          }
        }
      )
    )
  })

  /**
   * **Validates: Requirements 7.4**
   *
   * Sorting preserves the set of elements — the sorted output contains
   * exactly the same elements as the input (multiset equality).
   */
  it('sorted output contains exactly the same elements as input', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 50 }),
        sortCriteriaArb,
        (items, criteria) => {
          const sorted = sortItems(items, criteria)
          // Check that every item in sorted exists in items by reference
          const inputSet = new Set(items)
          for (const item of sorted) {
            expect(inputSet.has(item)).toBe(true)
          }
          // And vice versa
          const outputSet = new Set(sorted)
          for (const item of items) {
            expect(outputSet.has(item)).toBe(true)
          }
        }
      )
    )
  })

  /**
   * **Validates: Requirements 7.4**
   *
   * Sorting is idempotent — sorting an already-sorted array produces
   * the same result.
   */
  it('sorting is idempotent: sorting twice produces the same result', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 50 }),
        sortCriteriaArb,
        (items, criteria) => {
          const sorted1 = sortItems(items, criteria)
          const sorted2 = sortItems(sorted1, criteria)
          expect(sorted2).toEqual(sorted1)
        }
      )
    )
  })

  /**
   * **Validates: Requirements 7.4**
   *
   * filterItems never increases the length of the array and all output
   * items satisfy the predicate.
   */
  it('filtered output length is <= input length and all items satisfy predicate', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 100 }),
        fc.double({ min: -10000, max: 10000, noNaN: true }),
        (items, threshold) => {
          const predicate = (item: TestItem) => item.balance >= threshold
          const filtered = filterItems(items, predicate)

          expect(filtered.length).toBeLessThanOrEqual(items.length)
          for (const item of filtered) {
            expect(predicate(item)).toBe(true)
          }
        }
      )
    )
  })

  /**
   * **Validates: Requirements 7.4**
   *
   * filterAndSort combined operation: output length equals the number of
   * items passing the filter, and the output is correctly sorted.
   */
  it('filterAndSort produces correctly filtered and sorted output', () => {
    fc.assert(
      fc.property(
        fc.array(testItemArb, { minLength: 0, maxLength: 50 }),
        sortCriteriaArb,
        (items, criteria) => {
          const predicate = (item: TestItem) => item.active
          const result = filterAndSort(items, predicate, criteria)

          // Length matches filtered count
          const expectedCount = items.filter(predicate).length
          expect(result).toHaveLength(expectedCount)

          // All items satisfy predicate
          for (const item of result) {
            expect(item.active).toBe(true)
          }

          // Ordering is correct
          const { key, direction } = criteria
          const dirMultiplier = direction === 'asc' ? 1 : -1
          for (let i = 0; i < result.length - 1; i++) {
            const cmp = defaultCompare(result[i][key], result[i + 1][key])
            expect(dirMultiplier * cmp).toBeLessThanOrEqual(0)
          }
        }
      )
    )
  })
})
