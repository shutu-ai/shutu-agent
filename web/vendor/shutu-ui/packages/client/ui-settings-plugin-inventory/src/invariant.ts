/** Package-owned invariant companion. @module @shutu-ai/client-ui-settings-plugin-inventory/invariant */

/* jscpd:ignore-start */
import type { Context } from '@shutu-ai/cordis'
import type { InvariantInstaller } from '@shutu-ai/invariants'

const PACKAGE_NAME = '@shutu-ai/client-ui-settings-plugin-inventory'

/** Cordis companion plugin name. */
export const name = 'client-ui-settings-plugin-inventory-invariant'
/** Service required before the companion can reserve package ownership. */
export const inject = ['invariants']

/** No runtime invariant: this package owns a read-only Settings contribution. */
const install: InvariantInstaller = () => {}

/** Register this package's invariant companion. */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
/* jscpd:ignore-end */
