import type { MCPServer } from './api.js';

export type Fields = {
  delegatedIssuer: string;
  delegatedAudience: string;
  signingKey: string;
  entraIssuer: string;
  entraClientSecret: string;
  clientID: string;
  audience: string;
  scopes: string;
  authorizationEndpoint: string;
  tokenEndpoint: string;
  deviceEndpoint: string;
  keyID: string;
};
export type PageState = {
  selection: { readonly id: string };
  agentID: string;
  agentKeyID: string;
  server: MCPServer | null;
  fields: Fields;

  message: string;
  error: string;
};
export const initialState = (): PageState => ({
  selection: { id: '' },
  agentID: '',
  agentKeyID: '',
  server: null,
  fields: {
    delegatedIssuer: '',
    delegatedAudience: '',
    signingKey: '',
    entraIssuer: '',
    entraClientSecret: '',
    clientID: '',
    audience: '',
    scopes: '',
    authorizationEndpoint: '',
    tokenEndpoint: '',
    deviceEndpoint: '',
    keyID: ''
  },

  message: '',
  error: ''
});
