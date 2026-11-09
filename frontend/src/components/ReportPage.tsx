import React, { useEffect, useState } from 'react';

// Авторизация и отчёты — через bionicpro-auth (порт 8000) с сессионной cookie
// (credentials: 'include'). Токены фронтенду недоступны по построению.
const AUTH = process.env.REACT_APP_AUTH_URL || 'http://localhost:8000';

interface Me {
  sub: string;
  preferred_username?: string;
  email?: string;
  name?: string;
}

const ReportPage: React.FC = () => {
  const [me, setMe] = useState<Me | null>(null);
  const [checked, setChecked] = useState(false);
  const [loading, setLoading] = useState(false);
  const [reportUrl, setReportUrl] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetch(`${AUTH}/api/me`, { credentials: 'include' })
      .then(async (r) => (r.ok ? r.json() : Promise.reject(new Error('unauthorized'))))
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setChecked(true));
  }, []);

  const downloadReport = async () => {
    setError(null);
    setReportUrl(null);
    const from = (document.getElementById('from') as HTMLInputElement)?.value;
    const to = (document.getElementById('to') as HTMLInputElement)?.value;
    try {
      setLoading(true);
      // период по умолчанию: последние 7 обработанных дней
      const q = new URLSearchParams({
        from: from || '2026-01-01',
        to: to || '2026-01-07',
      });
      const response = await fetch(`${AUTH}/api/reports?${q}`, {
        credentials: 'include',
      });
      const body = await response.json();
      if (!response.ok) {
        throw new Error(body.error || `HTTP ${response.status}`);
      }
      setReportUrl(body.report_url);
      if (body.report_url) {
        window.open(body.report_url, '_blank', 'noopener');
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'An error occurred');
    } finally {
      setLoading(false);
    }
  };

  if (!checked) {
    return <div className="min-h-screen bg-gray-100"><p className="p-8">Loading...</p></div>;
  }

  if (!me) {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
        <div className="p-8 bg-white rounded-lg shadow-md">
          <h1 className="text-2xl font-bold mb-6">BionicPRO Reports</h1>
          <p className="mb-4 text-gray-600">Для доступа к отчётам войдите через SSO (PKCE, MFA).</p>
          <a
            href={`${AUTH}/login?redirect=/`}
            className="inline-block px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600"
          >
            Login
          </a>
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
      <div className="p-8 bg-white rounded-lg shadow-md w-full max-w-xl">
        <div className="flex justify-between items-start mb-6">
          <h1 className="text-2xl font-bold">Usage Reports</h1>
          <a href={`${AUTH}/logout`} className="text-sm text-blue-600 hover:underline">
            Logout ({me.preferred_username || me.sub})
          </a>
        </div>

        <div className="flex gap-4 mb-6">
          <label className="text-sm text-gray-700">
            С даты
            <input id="from" type="date" defaultValue="2026-01-01" className="block mt-1 border rounded px-2 py-1" />
          </label>
          <label className="text-sm text-gray-700">
            По дату
            <input id="to" type="date" defaultValue="2026-01-07" className="block mt-1 border rounded px-2 py-1" />
          </label>
        </div>

        <button
          onClick={downloadReport}
          disabled={loading}
          className={`px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600 ${
            loading ? 'opacity-50 cursor-not-allowed' : ''
          }`}
        >
          {loading ? 'Generating Report...' : 'Download Report'}
        </button>

        {reportUrl && (
          <div className="mt-4 p-4 bg-green-100 text-green-800 rounded break-all">
            Отчёт готов (S3 + CDN):{' '}
            <a className="underline" href={reportUrl} target="_blank" rel="noopener noreferrer">
              {reportUrl}
            </a>
          </div>
        )}

        {error && (
          <div className="mt-4 p-4 bg-red-100 text-red-700 rounded">{error}</div>
        )}
      </div>
    </div>
  );
};

export default ReportPage;
