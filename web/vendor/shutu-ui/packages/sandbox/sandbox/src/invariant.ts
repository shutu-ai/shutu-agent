/**
 * Package-owned invariant companion for `@shutu-ai/sandbox`.
 * @module @shutu-ai/sandbox/invariant
 */

/* jscpd:ignore-start */
import type { Context } from '@shutu-ai/cordis'
import type { InvariantInstaller } from '@shutu-ai/invariants'

const PACKAGE_NAME = '@shutu-ai/sandbox'

/** Cordis companion plugin name. */
export const name = 'sandbox-invariant'
/** Service required before the companion can reserve package ownership. */
export const inject = ['invariants']

/**
 * No runtime invariant: this package exposes no independent event sequence or mutable data relation
 * beyond contracts enforced at its owning seam.
 */
const install: InvariantInstaller = () => {}

/**
 * Register this package's invariant companion.
 * @param ctx - Cordis context carrying the invariant service.
 * @returns the installed registration's disposer after setup succeeds.
 */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
/* jscpd:ignore-end */
