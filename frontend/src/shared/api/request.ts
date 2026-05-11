import { fetchMgmtApi } from '@/features/admin-proxy/api'

export interface ApiCallRequest {
  method: string;
  url: string;
  header?: Record<string, string>;
  data?: string;
}

export interface ApiCallResult<T = unknown> {
  statusCode: number;
  header: Record<string, string[]>;
  bodyText: string;
  body: T | null;
}

export const apiCallApi = {
  request: async (payload: ApiCallRequest): Promise<ApiCallResult> => {
    const response = await fetchMgmtApi('/api-call', {
      method: 'POST',
      body: JSON.stringify(payload)
    });
    const data = typeof response === 'string' ? JSON.parse(response) : response;
    
    const statusCode = Number(data?.status_code ?? data?.statusCode ?? 0);
    const header = (data?.header ?? data?.headers ?? {}) as Record<string, string[]>;
    
    let bodyText = ''
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let body: any = null
    
    if (data?.body !== undefined) {
      if (typeof data.body === 'string') {
        bodyText = data.body;
        try {
          body = JSON.parse(bodyText);
        } catch {
          body = bodyText;
        }
      } else {
        body = data.body;
        try {
           bodyText = JSON.stringify(body);
        } catch {
           bodyText = String(body);
        }
      }
    }

    return {
      statusCode,
      header,
      bodyText,
      body
    };
  }
};
