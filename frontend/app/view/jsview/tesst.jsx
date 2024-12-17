import React, { useState, useEffect } from "https://esm.sh/react@18";
import ReactDOM from "https://esm.sh/react-dom@18";
import { setup as twindSetup } from 'https://cdn.skypack.dev/twind/shim';
import wmoCodeToEmoji from 'https://esm.sh/wmo-emoji';

twindSetup();

const WeatherApp = () => {
    const [weather, setWeather] = useState(null);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        const fetchWeather = async () => {
            setLoading(true);
            try {
                const response = await fetch("https://api.open-meteo.com/v1/forecast?latitude=44.1281&longitude=4.0839&current_weather=true");
                const data = await response.json();
                setWeather(data.current_weather);
            } catch (error) {
                console.error("Failed to fetch weather data:", error);
            } finally {
                setLoading(false);
            }
        };

        fetchWeather();
    }, []);

    const degToCompass = (num) => {
        var val = Math.floor((num / 22.5) + 0.5);
        var arr = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
        return arr[(val % 16)];
    };

    return (
        <div className="min-h-screen flex flex-col items-center justify-center">
            <h1 className="text-xl font-bold mb-4">Weather in Paris, France</h1>
            {loading ? <p>Loading...</p> : weather && (
                <div className="text-center">
                    <p>{wmoCodeToEmoji(weather.weathercode)}</p>
                    <p>Temperature: {weather.temperature}°C</p>
                    <p>Wind Speed: {weather.windspeed} km/h</p>
                    <p>Wind Direction: {degToCompass(weather.winddirection)}</p>
                </div>
            )}
        </div>
    );
};

ReactDOM.render(<WeatherApp />, document.getElementById("weather-app"));