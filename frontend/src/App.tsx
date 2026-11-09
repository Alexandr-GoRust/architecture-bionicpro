import React from 'react';
import ReportPage from './components/ReportPage';

// Задание 1, задача 3 (п.10): фронтенд больше не работает с Keycloak напрямую
// и не получает токены. Вся аутентификация — через бэкенд bionicpro-auth
// (сессионная cookie HttpOnly/Secure): ReactKeycloakProvider и keycloak-js
// удалены из приложения.
const App: React.FC = () => {
  return (
    <div className="App">
      <ReportPage />
    </div>
  );
};

export default App;
