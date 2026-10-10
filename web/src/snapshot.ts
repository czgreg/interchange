import type {
  Active,
  Healthz,
  NodesHealth,
  RefreshInterval,
  Status,
  Subscription,
  Transitions,
} from './types';

/** One endpoint's latest result: data on success, err (and HTTP status) on failure. */
export interface Slot<T> {
  data?: T;
  err?: string;
  status?: number;
}

/** Everything one polling round fetched. Pages render from this alone. */
export interface Snapshot {
  /** null = /healthz unreachable; undefined = not polled yet. */
  healthz?: { httpStatus: number; body: Healthz | null } | null;
  status: Slot<Status>;
  active: Slot<Active>;
  health: Slot<NodesHealth>;
  subs: Slot<Subscription[]>;
  interval: Slot<RefreshInterval>;
  trans: Slot<Transitions>;
  updatedAt?: Date;
}

export const emptySnapshot: Snapshot = {
  status: {},
  active: {},
  health: {},
  subs: {},
  interval: {},
  trans: {},
};
